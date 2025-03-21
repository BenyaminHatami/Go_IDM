package queue

import (
	"fmt"
	"math"
	"sync"
	"time"

	"GoParallelDownload/internal/download"
	"GoParallelDownload/internal/progress"
	"GoParallelDownload/internal/state"
	"GoParallelDownload/pkg/concurrency/ratelimit"
	"GoParallelDownload/pkg/concurrency/workerpool"
	"GoParallelDownload/pkg/types"
)

var Queues = make(map[int]*types.DownloadQueue)

// ProcessQueue now takes a WaitGroup to track individual tasks
func ProcessQueue(queue *types.DownloadQueue, results chan<- types.DownloadStatus, taskWg *sync.WaitGroup) {
	if err := setupQueue(queue); err != nil {
		fmt.Printf("Failed to setup queue %d: %v\n", queue.ID, err)
		return
	}
	queuePool := workerpool.NewWorkerPool(queue.ConcurrentLimit)
	startTimeCheck(queue)
	submitQueueTasks(queue, queuePool, results, taskWg)
}

func setupQueue(queue *types.DownloadQueue) error {
	if len(queue.Tasks) == 0 {
		return fmt.Errorf("queue has no tasks")
	}
	queue.TokenBucket = ratelimit.NewTokenBucket(queue.SpeedLimit, queue.SpeedLimit)
	fmt.Printf("Queue %d TokenBucket initialized with max %.1f KB/s\n", queue.ID, queue.SpeedLimit/1024)
	for i := range queue.Tasks {
		queue.Tasks[i].QueueID = queue.ID
	}
	Queues[queue.ID] = queue
	return nil
}

func UpdateQueueSpeedLimit(queueID int, newSpeedLimit float64) {
	if queue, exists := Queues[queueID]; exists {
		queue.SpeedLimit = newSpeedLimit
		queue.TokenBucket.Mu.Lock()
		queue.TokenBucket.MaxTokens = newSpeedLimit
		queue.TokenBucket.RefillRate = newSpeedLimit
		queue.TokenBucket.Tokens = math.Min(queue.TokenBucket.Tokens, newSpeedLimit)
		queue.TokenBucket.Mu.Unlock()
		fmt.Printf("Updated speed limit for Queue %d to %.1f KB/s\n", queueID, newSpeedLimit/1024)
	} else {
		fmt.Printf("Queue %d not found\n", queueID)
	}
}

func startTimeCheck(queue *types.DownloadQueue) {
	if queue.StartTime != nil || queue.StopTime != nil {
		go func() {
			for {
				checkQueueTimeWindow(queue)
				time.Sleep(10 * time.Second)
			}
		}()
	}
}

func checkQueueTimeWindow(queue *types.DownloadQueue) {
	now := time.Now()
	withinTimeWindow := true
	if queue.StartTime != nil {
		startToday := time.Date(now.Year(), now.Month(), now.Day(), queue.StartTime.Hour(), queue.StartTime.Minute(), 0, 0, time.Local)
		if now.Before(startToday) {
			withinTimeWindow = false
		}
	}
	if queue.StopTime != nil {
		stopToday := time.Date(now.Year(), now.Month(), now.Day(), queue.StopTime.Hour(), queue.StopTime.Minute(), 0, 0, time.Local)
		if now.After(stopToday) {
			withinTimeWindow = false
		}
	}
	controlQueueTasks(queue, withinTimeWindow)
}

func controlQueueTasks(queue *types.DownloadQueue, withinTimeWindow bool) {
	for _, task := range queue.Tasks {
		state.ControlMux.Lock()
		if controlChan, ok := state.ControlChans[task.ID]; ok {
			if withinTimeWindow {
				if status, exists := state.ProgressMap[task.FilePath]; exists && status.State == "paused" && status.Error == nil {
					controlChan <- types.ControlMessage{TaskID: task.ID, Action: "resume"}
					fmt.Printf("Resuming task %d due to time window\n", task.ID)
				}
			} else {
				if status, exists := state.ProgressMap[task.FilePath]; exists && status.State == "running" {
					controlChan <- types.ControlMessage{TaskID: task.ID, Action: "pause"}
					fmt.Printf("Pausing task %d due to time window\n", task.ID)
				}
			}
		}
		state.ControlMux.Unlock()
	}
}

func submitQueueTasks(queue *types.DownloadQueue, queuePool *workerpool.WorkerPool, results chan<- types.DownloadStatus, taskWg *sync.WaitGroup) {
	for _, task := range queue.Tasks {
		controlChan := make(chan types.ControlMessage, 10)
		state.ControlMux.Lock()
		state.ControlChans[task.ID] = controlChan
		state.ControlMux.Unlock()
		taskWg.Add(1) // Increment for each task
		queuePool.Submit(func() {
			defer taskWg.Done() // Decrement when task finishes
			download.DownloadFile(task, controlChan, results, queue)
			progress.CleanupTask(task)
		})
	}
}

func SendQueueControlMessage(queueID int, action string) {
	state.ControlMux.Lock()
	defer state.ControlMux.Unlock()
	if queue, exists := Queues[queueID]; exists {
		for _, task := range queue.Tasks {
			if controlChan, ok := state.ControlChans[task.ID]; ok {
				controlChan <- types.ControlMessage{TaskID: task.ID, QueueID: queueID, Action: action}
			}
		}
		fmt.Printf("%s command sent for all tasks in Queue %d\n", action, queueID)
	} else {
		fmt.Printf("Queue %d not found\n", queueID)
	}
}

func ApplyHardcodedSpeedChanges() {
	time.Sleep(3 * time.Second)
	SendQueueControlMessage(1, "resume")
	fmt.Println("Queue 1 all tasks resumed after 3 seconds")

	time.Sleep(5 * time.Second)
	UpdateQueueSpeedLimit(1, 600*1024)
	fmt.Println("Queue 1 speed limit updated to 600 KB/s after 8 seconds")

	time.Sleep(5 * time.Second)
	UpdateQueueSpeedLimit(1, 700*1024)
	fmt.Println("Queue 1 speed limit updated to 700 KB/s after 13 seconds")
}

func UpdateQueueTimeInterval(queueID int, startTime, stopTime *time.Time) {
	if queue, exists := Queues[queueID]; exists {
		queue.StartTime = startTime
		queue.StopTime = stopTime
		fmt.Printf("Updated time interval for Queue %d to Start: %s, Stop: %s\n",
			queueID, startTime.Format("15:04"), stopTime.Format("15:04"))
		checkQueueTimeWindow(queue)
	} else {
		fmt.Printf("Queue %d not found\n", queueID)
	}
}

func UpdateQueueRetries(queueID, retries int) {
	if retries < 0 {
		fmt.Println("Retries cannot be negative")
		return
	}
	if queue, exists := Queues[queueID]; exists {
		queue.MaxRetries = retries
		fmt.Printf("Updated max retries for Queue %d to %d\n", queueID, retries)
	} else {
		fmt.Printf("Queue %d not found\n", queueID)
	}
}

func ValidateQueue(queue *types.DownloadQueue) error {
	if queue.SpeedLimit <= 0 {
		return fmt.Errorf("speed limit must be positive")
	}
	if queue.ConcurrentLimit <= 0 {
		return fmt.Errorf("concurrent limit must be positive")
	}
	if queue.MaxRetries < 0 {
		return fmt.Errorf("max retries cannot be negative")
	}
	return nil
}
