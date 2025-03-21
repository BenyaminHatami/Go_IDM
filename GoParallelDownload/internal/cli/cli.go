package cli

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"GoParallelDownload/internal/progress"
	"GoParallelDownload/internal/queue"
	"GoParallelDownload/internal/state"
)

func HandleUserInput(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if !processCommand(scanner.Text()) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			fmt.Printf("Scanner error: %v\n", err)
		}
	}()
}

func processCommand(input string) bool {
	input = strings.TrimSpace(input)
	if input == "exit" {
		fmt.Println("Exiting...")
		// Cancel all active queues
		state.ControlMux.Lock()
		for queueID := range queue.Queues {
			queue.SendQueueControlMessage(queueID, "cancel")
		}
		state.ControlMux.Unlock()
		close(state.StopDisplay) // Stop progress display
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
		startTime, stopTime, err := parseTimeInterval(parts[2], parts[3])
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
		progress.SendControlMessage(taskID, -1, action)
	}
	return true
}

func parseTimeInterval(startStr, stopStr string) (*time.Time, *time.Time, error) {
	now := time.Now()
	startParts := strings.Split(startStr, ":")
	stopParts := strings.Split(stopStr, ":")
	if len(startParts) != 2 || len(stopParts) != 2 {
		return nil, nil, fmt.Errorf("invalid time format, use HH:MM")
	}

	startHour, err1 := strconv.Atoi(startParts[0])
	startMin, err2 := strconv.Atoi(startParts[1])
	stopHour, err3 := strconv.Atoi(stopParts[0])
	stopMin, err4 := strconv.Atoi(stopParts[1])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil ||
		startHour < 0 || startHour > 23 || startMin < 0 || startMin > 59 ||
		stopHour < 0 || stopHour > 23 || stopMin < 0 || stopMin > 59 {
		return nil, nil, fmt.Errorf("invalid time values, use HH:MM (00:00-23:59)")
	}

	start := time.Date(now.Year(), now.Month(), now.Day(), startHour, startMin, 0, 0, time.Local)
	stop := time.Date(now.Year(), now.Month(), now.Day(), stopHour, stopMin, 0, 0, time.Local)
	return &start, &stop, nil
}
