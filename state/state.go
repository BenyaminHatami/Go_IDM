package state

import (
	"sync"

	"GoParallelDownload/types"
)

var (
	ProgressMap  = make(map[string]types.DownloadStatus)
	ProgressMux  = sync.Mutex{}
	ControlChans = make(map[int]chan types.ControlMessage)
	ControlMux   = sync.Mutex{}
	StopDisplay  = make(chan struct{})
	ActiveTasks  = sync.Map{}
)
