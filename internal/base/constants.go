package base

import (
	"sync"
	"time"
)

const (
	MaxConcurrentDownloads = 3                      // Limit to 3 concurrent downloads
	BandwidthLimit         = 600 * 1024             // 300 KB/s (in bytes)
	TokenSize              = 1024                   // 1 KB per token
	DownloadDir            = "Downloads/"           // Folder to save files
	WindowDuration         = 1 * time.Second        // Rolling window for speed calculation
	UpdateIntervalBytes    = 10 * 1024              // Update progressMap every 10 KB
	UpdateIntervalTime     = 100 * time.Millisecond // Update progressMap every 100ms
	RefillInterval         = 10 * time.Millisecond  // Refill token bucket every 10ms
)

var (
	ProgressMap  = make(map[string]DownloadStatus)
	ProgressMux  = sync.Mutex{}
	ControlChans = make(map[int]chan ControlMessage)
	ControlMux   = sync.Mutex{}
)
