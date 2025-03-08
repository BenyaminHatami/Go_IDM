package main

import (
	"bufio"
	"fmt"
	"io"
	"myproject/internal/base"
	"myproject/internal/concurrencies"
	"myproject/internal/ui/download_tab"
	"myproject/internal/ui/queue_tab"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TokenBucket represents a token bucket system
/*
type TokenBucket struct {
	mu             sync.Mutex
	tokens         float64 // Current number of tokens
	maxTokens      float64 // Maximum tokens (capacity)
	refillRate     float64 // Tokens added per second (bytes/sec)
	lastRefillTime time.Time
	quit           chan struct{} // To stop the refill goroutine
}
*/

// Worker function to process download tasks
func worker(id int, jobs <-chan base.DownloadTask, results chan<- base.DownloadStatus, tokenBucket *concurrencies.TokenBucket, wg *sync.WaitGroup) {
	defer wg.Done()
	for task := range jobs {
		// Create a control channel for this task
		controlChan := make(chan base.ControlMessage)
		base.ControlMux.Lock()
		base.ControlChans[task.ID] = controlChan
		base.ControlMux.Unlock()

		// Run the download
		status := downloadFile(task, tokenBucket, controlChan)
		results <- status

		// Clean up the control channel
		base.ControlMux.Lock()
		delete(base.ControlChans, task.ID)
		close(controlChan)
		// Remove from progressMap if canceled
		if status.State == "canceled" {
			base.ProgressMux.Lock()
			delete(base.ProgressMap, task.FilePath)
			base.ProgressMux.Unlock()
		}
		base.ControlMux.Unlock()
	}
}

// downloadFile downloads a file with throttling
func downloadFile(task base.DownloadTask, tokenBucket *concurrencies.TokenBucket, controlChan chan base.ControlMessage) base.DownloadStatus {
	// Ensure Downloads directory exists
	if err := os.MkdirAll(base.DownloadDir+task.FileType, 0755); err != nil {
		return base.DownloadStatus{URL: task.URL, Error: err}
	}

	// Full path includes Downloads folder
	fullPath := filepath.Join(base.DownloadDir+task.FileType, task.FilePath)

	// Open the file for writing
	file, err := os.Create(fullPath)
	if err != nil {
		return base.DownloadStatus{URL: task.URL, Error: err}
	}
	defer file.Close()

	// Send an HTTP GET request
	resp, err := http.Get(task.URL)
	if err != nil {
		return base.DownloadStatus{URL: task.URL, Error: err}
	}
	defer resp.Body.Close()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		return base.DownloadStatus{URL: task.URL, Error: fmt.Errorf("bad status: %s", resp.Status)}
	}

	// Read the response body in chunks
	buffer := make([]byte, base.TokenSize)
	totalBytes := resp.ContentLength
	hasTotalBytes := totalBytes != -1
	bytesDownloaded := 0
	accumulatedBytes := 0 // For updating progressMap less frequently
	lastUpdate := time.Now()

	// Initialize status with window tracking
	status := base.DownloadStatus{
		ID:            task.ID,
		URL:           task.URL,
		TotalBytes:    totalBytes,
		HasTotalBytes: hasTotalBytes,
		BytesInWindow: []int{},
		Timestamps:    []time.Time{},
		State:         "running",
	}

	// Download loop with pause/resume/cancel support
	for {
		select {
		case msg := <-controlChan:
			if msg.TaskID != task.ID {
				continue
			}
			switch msg.Action {
			case "pause":
				// Calculate progress before pausing
				progress := 0
				if hasTotalBytes {
					progress = int(float64(bytesDownloaded) / float64(totalBytes) * 100)
				}
				// Update speed, ETA, and state before pausing
				var speed int
				var windowSeconds float64
				if len(status.Timestamps) > 1 {
					first := status.Timestamps[0]
					last := status.Timestamps[len(status.Timestamps)-1]
					windowSeconds = last.Sub(first).Seconds()
				} else if len(status.Timestamps) == 1 {
					windowSeconds = time.Since(status.Timestamps[0]).Seconds()
				}
				if windowSeconds > 0 {
					var totalBytesInWindow int
					for _, b := range status.BytesInWindow {
						totalBytesInWindow += b
					}
					speed = int(float64(totalBytesInWindow) / windowSeconds / 1024)
				} else {
					speed = 0
				}
				var eta string
				if hasTotalBytes && speed > 0 {
					remainingBytes := totalBytes - int64(bytesDownloaded)
					etaSeconds := float64(remainingBytes) / float64(speed*1024)
					etaDuration := time.Duration(etaSeconds * float64(time.Second))
					eta = etaDuration.Truncate(time.Second).String()
				} else {
					eta = "N/A"
				}

				// Update progressMap with the latest progress and paused state
				base.ProgressMux.Lock()
				base.ProgressMap[task.FilePath] = base.DownloadStatus{
					ID:            task.ID,
					URL:           task.URL,
					Progress:      progress,
					BytesDone:     bytesDownloaded,
					TotalBytes:    totalBytes,
					HasTotalBytes: hasTotalBytes,
					Speed:         speed,
					ETA:           eta,
					BytesInWindow: status.BytesInWindow,
					Timestamps:    status.Timestamps,
					State:         "paused",
				}
				base.ProgressMux.Unlock()

				// Wait until resumed or canceled
				for {
					resumeMsg := <-controlChan
					if resumeMsg.TaskID == task.ID {
						if resumeMsg.Action == "resume" {
							base.ProgressMux.Lock()
							base.ProgressMap[task.FilePath] = base.DownloadStatus{
								ID:            task.ID,
								URL:           task.URL,
								Progress:      progress,
								BytesDone:     bytesDownloaded,
								TotalBytes:    totalBytes,
								HasTotalBytes: hasTotalBytes,
								Speed:         speed,
								ETA:           eta,
								BytesInWindow: status.BytesInWindow,
								Timestamps:    status.Timestamps,
								State:         "running",
							}
							base.ProgressMux.Unlock()
							status.State = "running"
							break
						} else if resumeMsg.Action == "cancel" {
							// Close file and delete it
							file.Close()
							os.Remove(fullPath)
							// Update progressMap to reflect canceled state
							base.ProgressMux.Lock()
							base.ProgressMap[task.FilePath] = base.DownloadStatus{
								ID:            task.ID,
								URL:           task.URL,
								Progress:      progress,
								BytesDone:     bytesDownloaded,
								TotalBytes:    totalBytes,
								HasTotalBytes: hasTotalBytes,
								Speed:         0,
								ETA:           "N/A",
								BytesInWindow: status.BytesInWindow,
								Timestamps:    status.Timestamps,
								State:         "canceled",
							}
							base.ProgressMux.Unlock()
							return base.DownloadStatus{URL: task.URL, State: "canceled"}
						}
					}
				}
			case "cancel":
				// Calculate progress before canceling
				progress := 0
				if hasTotalBytes {
					progress = int(float64(bytesDownloaded) / float64(totalBytes) * 100)
				}
				// Close file and delete it
				file.Close()
				os.Remove(fullPath)
				// Update progressMap to reflect canceled state
				base.ProgressMux.Lock()
				base.ProgressMap[task.FilePath] = base.DownloadStatus{
					ID:            task.ID,
					URL:           task.URL,
					Progress:      progress,
					BytesDone:     bytesDownloaded,
					TotalBytes:    totalBytes,
					HasTotalBytes: hasTotalBytes,
					Speed:         0,
					ETA:           "N/A",
					BytesInWindow: status.BytesInWindow,
					Timestamps:    status.Timestamps,
					State:         "canceled",
				}
				base.ProgressMux.Unlock()
				return base.DownloadStatus{URL: task.URL, State: "canceled"}
			}
		default:
			if status.State != "running" {
				continue
			}

			// Wait for tokens before downloading
			tokenBucket.WaitForTokens(base.TokenSize)

			n, err := resp.Body.Read(buffer)
			if n > 0 {
				file.Write(buffer[:n])
				bytesDownloaded += n
				accumulatedBytes += n

				// Calculate progress (if total size is known)
				progress := 0
				if hasTotalBytes {
					progress = int(float64(bytesDownloaded) / float64(totalBytes) * 100)
				}

				// Update rolling window for speed calculation
				now := time.Now()
				status.BytesInWindow = append(status.BytesInWindow, n)
				status.Timestamps = append(status.Timestamps, now)

				// Remove entries outside the 1-second window
				cutoff := now.Add(-base.WindowDuration)
				newBytes := []int{}
				newTimes := []time.Time{}
				for i, ts := range status.Timestamps {
					if ts.After(cutoff) {
						newBytes = append(newBytes, status.BytesInWindow[i])
						newTimes = append(newTimes, ts)
					}
				}
				status.BytesInWindow = newBytes
				status.Timestamps = newTimes

				// Calculate speed over the window
				var totalBytesInWindow int
				for _, b := range status.BytesInWindow {
					totalBytesInWindow += b
				}
				var windowSeconds float64
				if len(status.Timestamps) > 1 {
					first := status.Timestamps[0]
					last := status.Timestamps[len(status.Timestamps)-1]
					windowSeconds = last.Sub(first).Seconds()
				} else {
					windowSeconds = time.Since(status.Timestamps[0]).Seconds()
				}
				var speed int
				if windowSeconds > 0 {
					speed = int(float64(totalBytesInWindow) / windowSeconds / 1024)
				} else {
					speed = 0
				}

				// Calculate ETA (if total size and speed are known)
				var eta string
				if hasTotalBytes && speed > 0 {
					remainingBytes := totalBytes - int64(bytesDownloaded)
					etaSeconds := float64(remainingBytes) / float64(speed*1024)
					etaDuration := time.Duration(etaSeconds * float64(time.Second))
					eta = etaDuration.Truncate(time.Second).String()
				} else {
					eta = "N/A"
				}

				// Update progressMap only if we've accumulated enough bytes or enough time has passed
				if accumulatedBytes >= base.UpdateIntervalBytes || time.Since(lastUpdate) >= base.UpdateIntervalTime {
					base.ProgressMux.Lock()
					base.ProgressMap[task.FilePath] = base.DownloadStatus{
						ID:            task.ID,
						URL:           task.URL,
						Progress:      progress,
						BytesDone:     bytesDownloaded,
						TotalBytes:    totalBytes,
						HasTotalBytes: hasTotalBytes,
						Speed:         speed,
						ETA:           eta,
						BytesInWindow: status.BytesInWindow,
						Timestamps:    status.Timestamps,
						State:         status.State,
					}
					base.ProgressMux.Unlock()
					accumulatedBytes = 0
					lastUpdate = now
				}
			}
			if err != nil {
				if err == io.EOF {
					// Final update to ensure 100% progress is shown TODO ???
					progress := 0
					if hasTotalBytes {
						progress = int(float64(bytesDownloaded) / float64(totalBytes) * 100)
					}
					base.ProgressMux.Lock()
					base.ProgressMap[task.FilePath] = base.DownloadStatus{
						ID:            task.ID,
						URL:           task.URL,
						Progress:      progress,
						BytesDone:     bytesDownloaded,
						TotalBytes:    totalBytes,
						HasTotalBytes: hasTotalBytes,
						Speed:         0,
						ETA:           "0s",
						BytesInWindow: status.BytesInWindow,
						Timestamps:    status.Timestamps,
						State:         "completed",
					}
					base.ProgressMux.Unlock()
					return base.DownloadStatus{URL: task.URL, Progress: 100, Speed: 0, State: "completed"}
				}
				return base.DownloadStatus{URL: task.URL, Error: err}
			}
		}
	}
}

func main() {
	// List of download tasks
	downloadTasks := []base.DownloadTask{
		{ID: 1, URL: "https://dl.sevilmusics.com/cdn/music/srvrf/Sohrab%20Pakzad%20-%20Dokhtar%20Irooni%20[SevilMusic]%20[Remix].mp3"},

		{ID: 2, URL: "https://soft1.downloadha.com/NarmAfzaar/June2020/Adobe.Photoshop.2020.v21.2.0.225.x64.Portable_www.Downloadha.com_.rar"},
		{ID: 3, URL: "https://dl6.dlhas.ir/behnam/2020/January/Android/Temple-Run-2-1.64.0-Arm64_Downloadha.com_.apk"},
		{ID: 4, URL: "https://dls.musics-fa.com/tagdl/downloads/Homayoun%20Shajarian%20-%20Chera%20Rafti%20(128).mp3"},
	}
	download_tab.SetFileNames(downloadTasks) // TODO should be in a better place! ya do ta ro bezae to ye tabe setType
	download_tab.SetFileType(downloadTasks)  // TODO should be in a better place!

	// Create channels for jobs and results
	jobs := make(chan base.DownloadTask, len(downloadTasks))
	results := make(chan base.DownloadStatus, len(downloadTasks))

	// Create a token bucket for throttling (300 KB/s)
	tokenBucket := concurrencies.NewTokenBucket(base.BandwidthLimit, base.BandwidthLimit)
	defer tokenBucket.Stop()

	// Start a fixed number of workers
	var wg sync.WaitGroup
	for w := 1; w <= base.MaxConcurrentDownloads; w++ {
		wg.Add(1)
		go worker(w, jobs, results, tokenBucket, &wg)
	}

	// Add download tasks to the job queue
	go func() {
		for _, task := range downloadTasks {
			jobs <- task
		}
		close(jobs)
	}()

	// Start the progress display
	go queue_tab.DisplayProgress()

	// Command-line input for pause/resume/cancel
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			input := strings.TrimSpace(scanner.Text())
			if input == "exit" {
				fmt.Println("Exiting...")
				os.Exit(0)
			}
			parts := strings.Split(input, " ")
			if len(parts) != 2 {
				fmt.Println("Invalid command. Use 'pause ID', 'resume ID', or 'cancel ID' (e.g., 'pause 1')")
				continue
			}
			var taskID int
			_, err := fmt.Sscanf(parts[1], "%d", &taskID)
			if err != nil {
				fmt.Println("Invalid task ID. Must be a number.")
				continue
			}
			base.ControlMux.Lock()
			if controlChan, ok := base.ControlChans[taskID]; ok {
				controlChan <- base.ControlMessage{TaskID: taskID, Action: parts[0]}
				fmt.Printf("%s command sent for task ID %d\n", parts[0], taskID)
			} else {
				fmt.Printf("Task ID %d not found or already completed/canceled\n", taskID)
			}
			base.ControlMux.Unlock()
		}
		if err := scanner.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "Error reading input: %v\n", err)
		}
	}()

	// Collect results
	go func() {
		for result := range results {
			if result.Error != nil {
				fmt.Printf("Download failed for %s: %v\n", result.URL, result.Error)
			} else if result.State == "canceled" {
				fmt.Printf("Download canceled for %s\n", result.URL)
			} else {
				fmt.Printf("Download completed for %s\n", result.URL)
			}
		}
	}()

	// Wait for all workers to finish
	wg.Wait()
	// Wait for a short time before updating again
	time.Sleep(500 * time.Millisecond)
	close(results)
}
