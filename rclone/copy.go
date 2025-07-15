package rclone

import (
	"context"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/sync"
)

func CopyFiles(
	ctx context.Context,
	fsrc fs.Fs, fdst fs.Fs, files []string,
) error {
	ctx = InjectConfig(ctx, files)
	return sync.CopyDir(ctx, fdst, fsrc, false)
}
