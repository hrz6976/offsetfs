package db

import (
	"gorm.io/gorm"
)

type Status int

const (
	Pending Status = iota
	Uploading
	Uploaded
	Downloading
	Downloaded
	Failed
)

// convert Status to string
func (s Status) String() string {
	switch s {
	case Pending:
		return "Pending"
	case Uploading:
		return "Uploading"
	case Uploaded:
		return "Uploaded"
	case Downloading:
		return "Downloading"
	case Downloaded:
		return "Downloaded"
	case Failed:
		return "Failed"
	default:
		return "Unknown"
	}
}

type Task struct {
	gorm.Model
	/* VirtualPath is the path in the S3 bucket and the virual file system.
	   It is unique and not null. */
	VirtualPath string `gorm:"uniqueIndex;not null"`
	/* SrcPath is the path of the file in the transfer source. */
	SrcPath string `gorm:"not null"`
	/* SrcSize is the size of the file in the transfer source. */
	SrcSize int64 `gorm:"not null"`
	/* SrcDigest is the sample_md5 digest of the file in the transfer source. */
	SrcDigest string `gorm:"nullable"`
	/* DstPath is the path of the file in the transfer destination. */
	DstPath string `gorm:"not null"`
	/* DstSize is the size of the file in the transfer destination. */
	DstSize int64 `gorm:"not null"`
	/* DstDigest is the sample_md5 digest of the file in the transfer destination. */
	DstDigest string `gorm:"nullable"`
	/* Status is the status of the task. */
	Status Status `gorm:"not null"`
	/* Error is the error message of the task. */
	Error string `gorm:"type:text"`
}
