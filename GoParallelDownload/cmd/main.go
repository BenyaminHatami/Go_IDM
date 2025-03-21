package main

import (
	"GoParallelDownload/internal/cli"
	"GoParallelDownload/internal/progress"
	"GoParallelDownload/internal/queue"
	"GoParallelDownload/pkg/concurrency/workerpool"
	"GoParallelDownload/pkg/types"
	"fmt"
	"sync"
	"time"
)

func main() {
	downloadQueues := setupDownloadQueues()
	results := make(chan types.DownloadStatus, 10)
	pool := workerpool.NewWorkerPool(2)
	var wg sync.WaitGroup

	progress.StartProgressDisplay(&wg)
	for _, q := range downloadQueues {
		pool.Submit(func() {
			queue.ProcessQueue(q, pool, results)
		})
		if err := queue.ValidateQueue(q); err != nil {
			fmt.Printf("Queue %d validation failed: %v\n", q.ID, err)
		}
	}
	cli.HandleUserInput(&wg, pool)

	totalTasks := 0
	for _, q := range downloadQueues {
		totalTasks += len(q.Tasks)
	}

	applyHardcodedSpeedChanges()
	progress.MonitorDownloads(&wg, results, totalTasks)

	pool.StopWait()
	close(results)
	wg.Wait()
	fmt.Println("All downloads processed.")
}

func setupDownloadQueues() []*types.DownloadQueue {
	queuesList := []*types.DownloadQueue{
		{
			Tasks: []types.DownloadTask{
				{ID: 1, URL: "https://dl.sevilmusics.com/cdn/music/srvrf/Sohrab%20Pakzad%20-%20Dokhtar%20Irooni%20[SevilMusic]%20[Remix].mp3"},
				{ID: 2, URL: "https://dls.musics-fa.com/tagdl/downloads/Homayoun%20Shajarian%20-%20Chera%20Rafti%20(128).mp3"},
				{ID: 3, URL: "https://soft1.downloadha.com/NarmAfzaar/June2020/Adobe.Photoshop.2020.v21.2.0.225.x64.Portable_www.Downloadha.com_.rar"},
				//{ID: 4, URL: "https://dl6.dlhas.ir/behnam/2020/January/Android/Temple-Run-2-1.64.0-Arm64_Downloadha.com_.apk"},
				{ID: 4, URL: "https://example.com/file3.mp3"},
			},
			SpeedLimit:       float64(400 * 1024), // 500 KB/s
			ConcurrentLimit:  2,
			StartTime:        nil,
			StopTime:         nil,
			MaxRetries:       2,
			ID:               1,
			DownloadLocation: "C:/CustomDownloads", // Example custom location
		},
		{
			Tasks: []types.DownloadTask{
				{ID: 6, URL: "https://soft1.downloadha.com/NarmAfzaar/June2020/Adobe.Photoshop.2020.v21.2.0.225.x64.Portable_www.Downloadha.com_.rar"},
				{ID: 7, URL: "https://dl6.dlhas.ir/behnam/2020/January/Android/Temple-Run-2-1.64.0-Arm64_Downloadha.com_.apk"},
			},
			SpeedLimit:       float64(300 * 1024), // 200 KB/s
			ConcurrentLimit:  2,
			StartTime:        nil,
			StopTime:         nil,
			MaxRetries:       2,
			ID:               2,
			DownloadLocation: "D:/AnotherLocation", // Another example custom location
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
		fmt.Println("Queue 1 speed limit updated to 600 KB/s after 7 seconds")

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
		fmt.Println("Queue 1 speed limit updated to 700 KB/s after 22 seconds")

		time.Sleep(5 * time.Second)
		queue.SendQueueControlMessage(1, "pause")
		fmt.Println("Queue 1 all tasks paused after 27 seconds")

		time.Sleep(5 * time.Second)
		queue.SendQueueControlMessage(1, "resume")
		fmt.Println("Queue 1 all tasks resumed again after 32 seconds")
	}()

}
