package progress

import (
	"GoParallelDownload/internal/state"
	"GoParallelDownload/pkg/types"
	"GoParallelDownload/pkg/utils"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func StartProgressDisplay(wg *sync.WaitGroup) {
	wg.Add(1)
	go displayProgress(wg)
}

func MonitorDownloads(wg *sync.WaitGroup, results <-chan types.DownloadStatus, totalTasks int) {
	wg.Add(1)
	defer wg.Done()

	completedTasks := 0
	for status := range results {
		// Update progress state as before
		if status.State == "completed" || status.State == "canceled" || status.State == "failed" {
			completedTasks++
			// Log completion
			if status.State == "completed" {
				fmt.Printf("Task %d (%s) completed\n", status.ID, status.URL)
			}
		}
		// No premature closing of results here; let main.go handle it
		if completedTasks >= totalTasks {
			break // Exit loop when all tasks are accounted for, but don’t close channel
		}
	}
	// Do NOT close(results) here; it’s handled in main.go after taskWg.Wait()
}

func SendControlMessage(taskID, queueID int, action string) {
	state.ControlMux.Lock()
	defer state.ControlMux.Unlock()
	if controlChan, ok := state.ControlChans[taskID]; ok {
		controlChan <- types.ControlMessage{TaskID: taskID, QueueID: queueID, Action: action}
		fmt.Printf("%s command sent for task ID %d\n", action, taskID)
	} else {
		fmt.Printf("Task ID %d not found or already completed/canceled\n", taskID)
	}
}

func ReportDownloadResult(result types.DownloadStatus) {
	if result.Error != nil {
		fmt.Printf("Download failed for %s after %d retries: %v\n", result.URL, result.RetriesLeft, result.Error)
	} else if result.State == "canceled" {
		fmt.Printf("Download canceled for %s\n", result.URL)
	} else {
		fmt.Printf("Download completed for %s\n", result.URL)
	}
}

func SetFileNames(tasks []types.DownloadTask) {
	duplicates := make(map[string]int)
	for i, dt := range tasks {
		if dt.FilePath == "" {
			duplicates[dt.URL]++
			temp := strings.Replace(filepath.Base(dt.URL), "%20", " ", -1)
			if duplicates[dt.URL] == 1 {
				tasks[i].FilePath = temp
			} else {
				temp1 := strings.LastIndex(temp, ".")
				tasks[i].FilePath = fmt.Sprintf("%s (%d)%s", temp[:temp1], duplicates[dt.URL]-1, temp[temp1:])
			}
		}
	}
}

func SetFileType(tasks []types.DownloadTask) {
	for i, dt := range tasks {
		temp2 := strings.LastIndex(dt.FilePath, ".")
		if temp2 == -1 {
			tasks[i].FileType = "General"
			continue
		}
		s := strings.ToLower(dt.FilePath[temp2+1:])
		switch s {
		case "mp3", "wav", "flac", "aac", "wma":
			tasks[i].FileType = "Music"
		case "mov", "avi", "mkv", "mp4":
			tasks[i].FileType = "Video"
		case "zip", "rar", "7z":
			tasks[i].FileType = "Compressed"
		case "jpeg", "jpg", "png":
			tasks[i].FileType = "Pictures"
		case "exe", "msi", "pkg":
			tasks[i].FileType = "Programs"
		case "pdf", "doc", "txt", "html":
			tasks[i].FileType = "Documents"
		default:
			tasks[i].FileType = "General"
		}
	}
}

func CleanupTask(task types.DownloadTask) {
	state.ControlMux.Lock()
	defer state.ControlMux.Unlock()

	if controlChan, exists := state.ControlChans[task.ID]; exists {
		delete(state.ControlChans, task.ID)
		close(controlChan)
	}

	state.ProgressMux.Lock()
	if status, ok := state.ProgressMap[task.FilePath]; ok && (status.State == "completed" || status.State == "canceled") {
		delete(state.ProgressMap, task.FilePath)
	}
	for i := 0; i < types.NumParts; i++ {
		delete(state.ProgressMap, fmt.Sprintf("%s.part%d", task.FilePath, i))
	}
	state.ProgressMux.Unlock()

	state.ActiveTasks.Delete(fmt.Sprintf("%d:%d", task.QueueID, task.ID))
	fmt.Printf("Cleaned up Task %d in Queue %d\n", task.ID, task.QueueID)
}

func displayProgress(wg *sync.WaitGroup) {
	defer wg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-state.StopDisplay:
			fmt.Println("\nProgress display stopped.")
			return
		case <-ticker.C:
			state.ProgressMux.Lock()
			if len(state.ProgressMap) == 0 {
				fmt.Println("No downloads.")
			} else {
				fmt.Println("Active Downloads:")
				hasActive := false
				for filePath, status := range state.ProgressMap {
					if status.PartID == -1 && status.State != "pending" {
						hasActive = true
						fmt.Printf("  %s (ID: %d): %s (%d KB/s) ETA: %s State: %s\n",
							filePath, status.ID, utils.ProgressBar(status), status.Speed, status.ETA, status.State)
						for i := 0; i < types.NumParts; i++ {
							partKey := fmt.Sprintf("%s.part%d", filePath, i)
							if partStatus, ok := state.ProgressMap[partKey]; ok {
								fmt.Printf("    Part %d: %s (%d KB/s) ETA: %s State: %s\n",
									i, utils.ProgressBar(partStatus), partStatus.Speed, partStatus.ETA, partStatus.State)
							}
						}
					}
				}
				if !hasActive {
					fmt.Println("  None")
				}

				fmt.Println("Pending Downloads:")
				hasPending := false
				for filePath, status := range state.ProgressMap {
					if status.PartID == -1 && status.State == "pending" {
						hasPending = true
						fmt.Printf("  %s (ID: %d): State: %s\n", filePath, status.ID, status.State)
					}
				}
				if !hasPending {
					fmt.Println("  None")
				}
			}
			state.ProgressMux.Unlock()
			fmt.Println("\nEnter command (e.g., 'pausequeue 1', 'resumequeue 1', 'cancelqueue 1', 'speed 1 600', 'settime 1 10:00 12:00', 'setretries 1 5', or 'exit'):")
		}
	}
}
