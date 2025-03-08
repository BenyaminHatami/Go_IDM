package base

import (
	"sync"
	"time"
)

// Global variables for progress tracking
var (
	progressMap  = make(map[string]DownloadStatus)
	progressMux  = sync.Mutex{}
	controlChans = make(map[int]chan ControlMessage)
	controlMux   = sync.Mutex{}
)

type DownloadState int

const (
	Failure DownloadState = iota
	Success
	InProgress
	Paused
)

type Queue struct {
	name                  string
	ConcurrencyLimitation int
	FilePath              string
	MaximumBandwidth      int
	StartTime             time.Time
	EndTime               time.Time
	MaxRetries            int
	tasks                 []DownloadTask
}

// DownloadTask represents a single download task
type DownloadTask struct {
	ID       int
	URL      string
	FileType string
	FilePath string
	Status   DownloadState
}

// DownloadStatus represents the status of a download
type DownloadStatus struct {
	ID            int
	URL           string
	Progress      int
	BytesDone     int    // Bytes downloaded so far
	TotalBytes    int64  // Total file size (if known)
	HasTotalBytes bool   // Whether totalBytes is known
	State         string // "running", "paused", "canceled", "completed"
	Speed         int    // Speed in KB/s
	ETA           string // Estimated time of arrival
	BytesInWindow []int  // Bytes downloaded in recent window
	Timestamps    []time.Time
	Error         error
}

// ControlMessage represents a control command
type ControlMessage struct {
	TaskID int
	Action string // "pause", "resume", or "cancel"
}

func (ds DownloadState) String() string {
	return [...]string{"Failure", "Success", "InProgress", "Paused"}[ds]
}

func InitializeQueue() {
	return
}
