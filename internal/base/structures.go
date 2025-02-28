package base

import (
	"time"
)

type DownloadState int

const (
	Failure DownloadState = iota
	Success
	InProgress
	Paused
)

type Queue struct {
	name                   string
	concurrency_limitation int
	file_path              string
	maximum_bandwidth      int
	start_time             time.Time
	end_time               time.Time
	max_retries            int
}

type DownloadItem struct {
	id        int
	state     DownloadState
	queue     Queue
	file_name string
}

func (ds DownloadState) String() string {
	return [...]string{"Failure", "Success", "InProgress", "Paused"}[ds]
}
