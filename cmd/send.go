package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/hrz6976/syncmate/db"
	"github.com/hrz6976/syncmate/logger"
	of "github.com/hrz6976/syncmate/offsetfs"
	"github.com/hrz6976/syncmate/rclone"
	"github.com/hrz6976/syncmate/woc"
	"github.com/rclone/rclone/fs"
	"github.com/spf13/cobra"
	"github.com/winfsp/cgofuse/fuse"
)

type CloudflareCredentials struct {
	// Explicitly define the fields to avoid duplicate json tags
	AccountID   string `json:"account_id"`
	ApiToken    string `json:"api_token,omitempty"`
	AccessKeyID string `json:"access_key_id,omitempty"`
	SecretKey   string `json:"secret_key,omitempty"`
	Bucket      string `json:"bucket,omitempty"`
	DatabaseID  string `json:"database_id,omitempty"`
}

var dbHandle *db.DB
var config *CloudflareCredentials

func connectDB() (*db.DB, error) {
	if dbHandle != nil {
		return dbHandle, nil
	}
	cloudflareD1Creds := db.CloudflareD1Credentials{
		APIToken:   config.ApiToken,
		DatabaseID: config.DatabaseID,
		AccountID:  config.AccountID,
	}
	gormDB, err := db.ConnectDB(cloudflareD1Creds)
	if err != nil {
		return nil, err
	}
	dbHandle = db.NewDB(gormDB)
	return dbHandle, nil
}

func generateTasks(
	srcProfile,
	dstProfile *woc.ParsedWocProfile) (map[string]*woc.WocSyncTask, error) {
	tasksMap := woc.GenerateFileLists(dstProfile, srcProfile)
	if dbHandle == nil {
		return nil, fmt.Errorf("Database connection not initialized")
	}
	finishedFiles, err := dbHandle.ListFinishedVirtualPaths()
	if err != nil {
		return nil, err
	}
	finishedFilesMap := make(map[string]bool)
	for _, file := range finishedFiles {
		finishedFilesMap[file] = true
	}
	for _, task := range tasksMap {
		if task.VirtualPath != "" && finishedFilesMap[task.VirtualPath] {
			delete(tasksMap, task.VirtualPath)
		}
	}
	return tasksMap, nil
}

func runSend(
	tasksMap map[string]*woc.WocSyncTask,
) error {
	// 1. Populate the remote database
	if dbHandle == nil {
		return fmt.Errorf("Database connection not initialized")
	}
	for _, task := range tasksMap {
		if err := dbHandle.UpdateTask(&db.Task{
			VirtualPath: task.VirtualPath,
			Status:      db.Uploading,
			SrcDigest:   *task.SourceDigest,
			DstDigest:   *task.TargetDigest,
		}); err != nil {
			return fmt.Errorf("failed to upsert task %s: %w", task.VirtualPath, err)
		}
	}

	// 2. Mount OffsetFS (don't block the main thread, listen to signals)
	offsetConfigs := make(map[string]*of.FileConfig)
	for _, task := range tasksMap {
		offsetConfigs[task.VirtualPath] = &of.FileConfig{
			VirtualPath: task.VirtualPath,
			SourcePath:  task.SourcePath,
			Offset:      task.Offset,
			Size:        task.Size,
		}
	}

	mountpoint := "/tmp/syncmate_offsetfs"
	if err := os.MkdirAll(mountpoint, 0755); err != nil {
		return fmt.Errorf("failed to create mountpoint: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	filesystem := of.NewOffsetFS(offsetConfigs, true) // 只读模式
	host := fuse.NewFileSystemHost(filesystem)

	options := []string{
		"-o", "fsname=syncmate_offsetfs",
		"-o", "volname=SyncMate OffsetFS",
	}

	var mountWg sync.WaitGroup
	mountWg.Add(1)
	mountSuccess := make(chan bool, 1)

	go func() {
		defer mountWg.Done()

		logger.Info("Mounting OffsetFS at %s...", mountpoint)

		if !host.Mount(mountpoint, options) {
			logger.Error("Failed to mount OffsetFS", "mountpoint", mountpoint)
			mountSuccess <- false
			return
		}

		logger.Info("OffsetFS mounted successfully", "mountpoint", mountpoint, "files", len(offsetConfigs))
		mountSuccess <- true

		<-ctx.Done()
		logger.Info("Unmounting OffsetFS...")
		host.Unmount()
		logger.Info("OffsetFS unmounted successfully")
	}()

	select {
	case success := <-mountSuccess:
		if !success {
			cancel()
			return fmt.Errorf("failed to mount OffsetFS")
		}
	case <-time.After(10 * time.Second):
		cancel()
		return fmt.Errorf("timeout waiting for OffsetFS mount")
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	taskDone := make(chan bool, 1)

	go func() {
		select {
		case <-sigChan:
			logger.Info("Received interrupt signal, cleaning up...")
			cancel()
		case <-taskDone:
			logger.Info("Tasks completed, cleaning up...")
			cancel()
		}
	}()

	go func() {
		defer func() {
			taskDone <- true
		}()

		logger.Info("Starting sync tasks...")

		// 创建R2后端
		r2Creds := &rclone.CloudflareR2Credentials{
			AccessKey: config.AccessKeyID,
			SecretKey: config.SecretKey,
			AccountID: config.AccountID,
			Bucket:    config.Bucket,
		}

		syncCtx := rclone.InjectGlobalConfig(ctx)
		fdst, err := rclone.NewR2Backend(syncCtx, r2Creds)
		if err != nil {
			logger.Error("Failed to create R2 backend", "error", err)
			return
		}

		// 检查是否被中断
		select {
		case <-ctx.Done():
			logger.Info("Upload cancelled before creating local filesystem")
			return
		default:
		}

		fsrc, err := fs.NewFs(syncCtx, mountpoint)
		if err != nil {
			logger.Error("Failed to create local filesystem", "error", err)
			return
		}

		// 准备要上传的文件列表
		var fileList []string
		for virtualPath := range offsetConfigs {
			fileList = append(fileList, virtualPath)
		}

		if len(fileList) == 0 {
			logger.Info("No files to upload")
			return
		}

		logger.Info("Uploading files to R2...", "count", len(fileList))

		// 再次检查是否被中断
		select {
		case <-ctx.Done():
			logger.Info("Upload cancelled before starting file transfer")
			return
		default:
		}

		// 执行文件上传，传递可取消的context
		uploadCtx := rclone.InjectFileList(syncCtx, fileList)

		// 创建一个用于监控上传进度的channel
		uploadDone := make(chan error, 1)

		// 在单独的goroutine中执行上传
		go func() {
			uploadDone <- rclone.CopyFiles(uploadCtx, fsrc, fdst, fileList)
		}()

		// 等待上传完成或被中断
		select {
		case err := <-uploadDone:
			if err != nil {
				logger.Error("File upload failed", "error", err)
				return
			}
			logger.Info("File upload completed successfully")
		case <-ctx.Done():
			logger.Info("Upload cancelled by user interrupt")
			// 这里可以添加清理逻辑，比如取消正在进行的上传
			return
		}

		// 更新数据库状态为完成
		if dbHandle != nil {
			logger.Info("Updating task status in database...")
			for _, task := range tasksMap {
				// 再次检查是否被中断
				select {
				case <-ctx.Done():
					logger.Info("Database update cancelled by user interrupt")
					return
				default:
				}

				if err := dbHandle.UpdateTask(&db.Task{
					VirtualPath: task.VirtualPath,
					Status:      db.Uploaded,
					SrcDigest:   *task.SourceDigest,
					DstDigest:   *task.TargetDigest,
				}); err != nil {
					logger.Error("Failed to update task status in database", "virtualPath", task.VirtualPath, "error", err)
				}
			}
		}

		logger.Info("Sync tasks completed successfully")
	}()

	mountWg.Wait()

	return nil
}

var sendCmd = &cobra.Command{
	Use:   "send",
	Short: "Upload files to S3-compatible storage",
	Long:  "Upload files to S3-compatible storage",
	Run: func(cmd *cobra.Command, args []string) {
		srcPath, _ := cmd.Flags().GetString("src")
		dstPath, _ := cmd.Flags().GetString("dst")
		configPath, _ := cmd.Flags().GetString("config")

		if srcPath == "" || dstPath == "" || configPath == "" {
			cmd.Help()
			return
		}

		if configData, err := os.ReadFile(configPath); err != nil {
			cmd.PrintErrf("Failed to read config file %s: %v\n", configPath, err)
			return
		} else {
			if err := json.Unmarshal(configData, &config); err != nil {
				cmd.PrintErrf("Failed to parse config file %s: %v\n", configPath, err)
				return
			}
		}

		_, err := connectDB() // 连接数据库
		if err != nil {
			cmd.PrintErrf("Failed to connect to database: %v\n", err)
			return
		}

		srcProfile, err := woc.ParseWocProfile(&srcPath)
		if err != nil {
			cmd.PrintErrf("Failed to parse source profile: %v\n", err)
			return
		}
		dstProfile, err := woc.ParseWocProfile(&dstPath)
		if err != nil {
			cmd.PrintErrf("Failed to parse destination profile: %v\n", err)
			return
		}

		tasksMap, err := generateTasks(srcProfile, dstProfile)
		if err != nil {
			cmd.PrintErrf("Failed to generate tasks: %v\n", err)
			return
		}

		logger.Info("Sync tasks generated", "count", len(tasksMap))

		if len(tasksMap) > 0 {
			if err := runSend(tasksMap); err != nil {
				cmd.PrintErrf("Failed to run send operation: %v\n", err)
				return
			}
		} else {
			logger.Info("No tasks to execute")
		}
	},
}

func init() {
	sendCmd.Flags().StringP("src", "s", "woc.src.json", "WoC profile of the transfer source")
	sendCmd.Flags().StringP("dst", "d", "woc.dst.json", "Woc profile of the transfer destination")
	sendCmd.Flags().StringP("config", "c", "config.json", "Path to the configuration file")
	sendCmd.MarkFlagRequired("src")
	sendCmd.MarkFlagRequired("dst")
	sendCmd.MarkFlagRequired("config")
	RootCmd.AddCommand(sendCmd)
}
