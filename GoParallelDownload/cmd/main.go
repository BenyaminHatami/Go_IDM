package main

import (
	"GoParallelDownload/internal/cli"
	"GoParallelDownload/internal/progress"
	"GoParallelDownload/internal/queue"
	"GoParallelDownload/pkg/types"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

var taskWg sync.WaitGroup // Global WaitGroup to track individual tasks

func main() {
	downloadQueues, err := loadConfig("config.json")
	if err != nil {
		fmt.Printf("Failed to load config: %v. Falling back to hardcoded queues.\n", err)
		downloadQueues = setupDownloadQueues()
	}

	results := make(chan types.DownloadStatus, 10)
	var wg sync.WaitGroup

	progress.StartProgressDisplay(&wg)

	totalTasks := 0
	for _, q := range downloadQueues {
		totalTasks += len(q.Tasks)
		if err := queue.ValidateQueue(q); err != nil {
			fmt.Printf("Queue %d validation failed: %v\n", q.ID, err)
			continue
		}
		go queue.ProcessQueue(q, results, &taskWg)
	}

	go cli.HandleUserInput(&wg)
	applyHardcodedSpeedChanges()
	go progress.MonitorDownloads(&wg, results, totalTasks) // Run in goroutine to not block

	wg.Wait()      // Wait for progress display and CLI to finish
	taskWg.Wait()  // Wait for all download tasks to complete
	close(results) // Close results only after all tasks are done
	fmt.Println("All downloads processed.")
}

// loadConfig reads and parses the configuration file into DownloadQueue structs
func loadConfig(filename string) ([]*types.DownloadQueue, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("could not open config file: %v", err)
	}
	defer file.Close()

	var configQueues []struct {
		ID               int                  `json:"id"`
		SpeedLimitKB     float64              `json:"speed_limit_kb"`
		ConcurrentLimit  int                  `json:"concurrent_limit"`
		MaxRetries       int                  `json:"max_retries"`
		DownloadLocation string               `json:"download_location"`
		Tasks            []types.DownloadTask `json:"tasks"`
	}
	if err := json.NewDecoder(file).Decode(&configQueues); err != nil {
		return nil, fmt.Errorf("could not decode config file: %v", err)
	}

	queues := make([]*types.DownloadQueue, len(configQueues))
	for i, cq := range configQueues {
		queues[i] = &types.DownloadQueue{
			ID:               cq.ID,
			SpeedLimit:       cq.SpeedLimitKB * 1024, // Convert KB/s to bytes/s
			ConcurrentLimit:  cq.ConcurrentLimit,
			MaxRetries:       cq.MaxRetries,
			DownloadLocation: cq.DownloadLocation,
			Tasks:            cq.Tasks,
			StartTime:        nil, // Not included in config yet; can be added later
			StopTime:         nil,
		}
		// Set QueueID for each task
		for j := range queues[i].Tasks {
			queues[i].Tasks[j].QueueID = cq.ID
		}
		progress.SetFileNames(queues[i].Tasks)
		progress.SetFileType(queues[i].Tasks)
	}
	return queues, nil
}

// setupDownloadQueues remains as a fallback or example
func setupDownloadQueues() []*types.DownloadQueue {
	queuesList := []*types.DownloadQueue{
		{
			Tasks: []types.DownloadTask{
				{ID: 1, URL: "https://dl.sevilmusics.com/cdn/music/srvrf/Sohrab%20Pakzad%20-%20Dokhtar%20Irooni%20[SevilMusic]%20[Remix].mp3"},
				{ID: 2, URL: "https://dls.musics-fa.com/tagdl/downloads/Homayoun%20Shajarian%20-%20Chera%20Rafti%20(128).mp3"},
				{ID: 3, URL: "https://soft1.downloadha.com/NarmAfzaar/June2020/Adobe.Photoshop.2020.v21.2.0.225.x64.Portable_www.Downloadha.com_.rar"},
				{ID: 4, URL: "https://example.com/file3.mp3"},
			},
			SpeedLimit:       float64(400 * 1024), // 400 KB/s
			ConcurrentLimit:  2,
			StartTime:        nil,
			StopTime:         nil,
			MaxRetries:       2,
			ID:               1,
			DownloadLocation: "C:/CustomDownloads",
		},
		{
			Tasks: []types.DownloadTask{
				{ID: 6, URL: "https://soft1.downloadha.com/NarmAfzaar/June2020/Adobe.Photoshop.2020.v21.2.0.225.x64.Portable_www.Downloadha.com_.rar"},
				{ID: 7, URL: "https://dl6.dlhas.ir/behnam/2020/January/Android/Temple-Run-2-1.64.0-Arm64_Downloadha.com_.apk"},
			},
			SpeedLimit:       float64(300 * 1024), // 300 KB/s
			ConcurrentLimit:  2,
			StartTime:        nil,
			StopTime:         nil,
			MaxRetries:       2,
			ID:               2,
			DownloadLocation: "D:/AnotherLocation",
		},
	}
	for i := range queuesList {
		progress.SetFileNames(queuesList[i].Tasks)
		progress.SetFileType(queuesList[i].Tasks)
	}
	return queuesList
}

func applyHardcodedSpeedChanges() {
	go func() {
		time.Sleep(2 * time.Second)
		queue.SendQueueControlMessage(1, "resume")
		queue.SendQueueControlMessage(2, "resume")
		fmt.Println("Queue 1 all tasks resumed after 2 seconds")

		time.Sleep(5 * time.Second)
		queue.UpdateQueueSpeedLimit(1, 500*1024)
		fmt.Println("Queue 1 speed limit updated to 500 KB/s after 7 seconds")

		time.Sleep(5 * time.Second)
		now := time.Now()
		startTime := now
		stopTime := now.Add(3 * time.Minute)
		queue.UpdateQueueTimeInterval(1, &startTime, &stopTime)
		fmt.Println("Queue 1 time interval updated to now-now+3min after 12 seconds")

		time.Sleep(5 * time.Second)
		queue.UpdateQueueRetries(1, 5)
		fmt.Println("Queue 1 max retries updated to 5 after 17 seconds")

		time.Sleep(5 * time.Second)
		queue.UpdateQueueSpeedLimit(1, 800*1024)
		fmt.Println("Queue 1 speed limit updated to 800 KB/s after 22 seconds")

		time.Sleep(5 * time.Second)
		queue.SendQueueControlMessage(1, "pause")
		fmt.Println("Queue 1 all tasks paused after 27 seconds")

		time.Sleep(5 * time.Second)
		queue.SendQueueControlMessage(1, "resume")
		fmt.Println("Queue 1 all tasks resumed again after 32 seconds")
	}()
}
