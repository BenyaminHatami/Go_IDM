package cmd

import (
	"GoParallelDownload/downloader"
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"GoParallelDownload/pool"
	"GoParallelDownload/queue"
	"GoParallelDownload/types"
)

func StartProgressDisplay(wg *sync.WaitGroup) {
	wg.Add(1)
	go downloader.DisplayProgress(wg)
}

func HandleUserInput(wg *sync.WaitGroup, pool *pool.WorkerPool) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if !processCommand(scanner.Text(), pool) {
				return
			}
		}
	}()
}

func MonitorDownloads(wg *sync.WaitGroup, results chan types.DownloadStatus, totalTasks int) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		count := 0
		for result := range results {
			if result.PartID == -1 {
				downloader.ReportDownloadResult(result)
				count++
				if count == totalTasks {
					close(types.StopDisplay)
				}
			}
		}
	}()
}

func processCommand(input string, pool *pool.WorkerPool) bool {
	input = strings.TrimSpace(input)
	if input == "exit" {
		fmt.Println("Exiting...")
		pool.StopWait()
		close(types.StopDisplay)
		return false
	}
	parts := strings.Split(input, " ")
	if len(parts) < 2 {
		fmt.Println("Invalid command. Use 'pausequeue QUEUE_ID', 'resumequeue QUEUE_ID', 'cancelqueue QUEUE_ID', 'speed QUEUE_ID KB_PER_SEC', 'settime QUEUE_ID START_HH:MM STOP_HH:MM', 'setretries QUEUE_ID NUM', or 'exit'")
		return true
	}

	action := parts[0]
	switch action {
	case "pausequeue", "resumequeue", "cancelqueue":
		queueID, err := strconv.Atoi(parts[1])
		if err != nil {
			fmt.Println("Invalid queue ID. Must be a number.")
			return true
		}
		action = strings.TrimPrefix(action, "queue")
		queue.SendQueueControlMessage(queueID, action)
	case "speed":
		if len(parts) != 3 {
			fmt.Println("Invalid speed command. Use 'speed QUEUE_ID KB_PER_SEC' (e.g., 'speed 1 600')")
			return true
		}
		queueID, err := strconv.Atoi(parts[1])
		if err != nil {
			fmt.Println("Invalid queue ID. Must be a number.")
			return true
		}
		speedKB, err := strconv.Atoi(parts[2])
		if err != nil {
			fmt.Println("Invalid speed. Must be a number (KB/s).")
			return true
		}
		queue.UpdateQueueSpeedLimit(queueID, float64(speedKB*1024))
	case "settime":
		if len(parts) != 4 {
			fmt.Println("Invalid settime command. Use 'settime QUEUE_ID START_HH:MM STOP_HH:MM' (e.g., 'settime 1 10:00 12:00')")
			return true
		}
		queueID, err := strconv.Atoi(parts[1])
		if err != nil {
			fmt.Println("Invalid queue ID. Must be a number.")
			return true
		}
		startTime, stopTime, err := downloader.ParseTimeInterval(parts[2], parts[3])
		if err != nil {
			fmt.Println("Invalid time format. Use HH:MM (e.g., 10:00)")
			return true
		}
		queue.UpdateQueueTimeInterval(queueID, startTime, stopTime)
	case "setretries":
		if len(parts) != 3 {
			fmt.Println("Invalid setretries command. Use 'setretries QUEUE_ID NUM' (e.g., 'setretries 1 5')")
			return true
		}
		queueID, err := strconv.Atoi(parts[1])
		if err != nil {
			fmt.Println("Invalid queue ID. Must be a number.")
			return true
		}
		retries, err := strconv.Atoi(parts[2])
		if err != nil {
			fmt.Println("Invalid retries number. Must be a non-negative integer.")
			return true
		}
		queue.UpdateQueueRetries(queueID, retries)
	default:
		if len(parts) != 2 {
			fmt.Println("Invalid command. Use 'pause ID', 'resume ID', 'cancel ID', 'reset ID', or queue commands")
			return true
		}
		taskID, err := strconv.Atoi(parts[1])
		if err != nil {
			fmt.Println("Invalid task ID. Must be a number.")
			return true
		}
		if action != "pause" && action != "resume" && action != "cancel" && action != "reset" {
			fmt.Println("Invalid action. Use 'pause', 'resume', 'cancel', 'reset', or queue commands.")
			return true
		}
		downloader.SendControlMessage(taskID, -1, action)
	}
	return true
}
