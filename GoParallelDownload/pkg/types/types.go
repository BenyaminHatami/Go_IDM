package types

import (
	"GoParallelDownload/pkg/concurrency/ratelimit"
	"time"
)

const (
	TotalBandwidthLimit = 500 * 1024 // Moved from progress
	TokenSize           = 256        // Moved from progress
	SubDirDownloads     = "Downloads/"
	NumParts            = 4
)

type DownloadTask struct {
	ID       int
	URL      string
	FilePath string
	FileType string
	QueueID  int
}

type DownloadQueue struct {
	Tasks            []DownloadTask
	SpeedLimit       float64
	ConcurrentLimit  int
	StartTime        *time.Time
	StopTime         *time.Time
	MaxRetries       int
	ID               int
	DownloadLocation string
	TokenBucket      *ratelimit.TokenBucket
}

type DownloadStatus struct {
	ID            int
	URL           string
	Progress      int
	BytesDone     int
	TotalBytes    int64
	HasTotalBytes bool
	State         string
	Speed         int
	ETA           string
	BytesInWindow []int
	Timestamps    []time.Time
	Error         error
	RetriesLeft   int
	PartID        int
}

type ControlMessage struct {
	TaskID  int
	QueueID int
	Action  string
}

type DownloadPart struct {
	Start int64
	End   int64
	Path  string
}
