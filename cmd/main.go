package main

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DownloadTask represents a single download task
type DownloadTask struct {
	ID       int
	URL      string
	FilePath string
	FileType string
}

// DownloadStatus represents the status of a download
type DownloadStatus struct {
	ID            int
	URL           string
	Progress      int
	BytesDone     int    // Bytes downloaded so far
	TotalBytes    int64  // Total file size (if known)
	HasTotalBytes bool   // Whether totalBytes is known
	State         string // "running", "paused", "canceled", "completed"
	Speed         int    // Speed in KB/s
	ETA           string // Estimated time of arrival
	BytesInWindow []int  // Bytes downloaded in recent window
	Timestamps    []time.Time
	Error         error
}

// ControlMessage represents a control command
type ControlMessage struct {
	TaskID int
	Action string // "pause", "resume", or "cancel"
}

// Constants
const (
	maxConcurrentDownloads = 3                      // Limit to 3 concurrent downloads
	bandwidthLimit         = 600 * 1024             // 300 KB/s (in bytes)
	tokenSize              = 1024                   // 1 KB per token
	downloadDir            = "Downloads/"           // Folder to save files
	windowDuration         = 1 * time.Second        // Rolling window for speed calculation
	updateIntervalBytes    = 10 * 1024              // Update progressMap every 10 KB
	updateIntervalTime     = 100 * time.Millisecond // Update progressMap every 100ms
	refillInterval         = 10 * time.Millisecond  // Refill token bucket every 10ms
)

// Global variables for progress tracking
var (
	progressMap  = make(map[string]DownloadStatus)
	progressMux  = sync.Mutex{}
	controlChans = make(map[int]chan ControlMessage)
	controlMux   = sync.Mutex{}
)

// TokenBucket represents a token bucket system
type TokenBucket struct {
	mu             sync.Mutex
	tokens         float64 // Current number of tokens
	maxTokens      float64 // Maximum tokens (capacity)
	refillRate     float64 // Tokens added per second (bytes/sec)
	lastRefillTime time.Time
	quit           chan struct{} // To stop the refill goroutine
}

// NewTokenBucket creates a new TokenBucket instance and starts the refill goroutine
func NewTokenBucket(maxTokens float64, refillRate float64) *TokenBucket {
	tb := &TokenBucket{
		tokens:         maxTokens,
		maxTokens:      maxTokens,
		refillRate:     refillRate,
		lastRefillTime: time.Now(),
		quit:           make(chan struct{}),
	}
	// Start the refill goroutine
	go tb.startRefill()
	return tb
}

// startRefill runs a goroutine to periodically refill the token bucket
func (tb *TokenBucket) startRefill() {
	ticker := time.NewTicker(refillInterval)
	defer ticker.Stop()
	for {
		select {
		case <-tb.quit:
			return
		case <-ticker.C:
			tb.mu.Lock()
			now := time.Now()
			duration := now.Sub(tb.lastRefillTime).Seconds()
			tokensToAdd := tb.refillRate * duration
			tb.tokens = math.Min(tb.tokens+tokensToAdd, tb.maxTokens)
			tb.lastRefillTime = now
			tb.mu.Unlock()
		}
	}
}

// Stop stops the refill goroutine
func (tb *TokenBucket) Stop() {
	close(tb.quit)
}

// Request checks if enough tokens are available and deducts them if so
func (tb *TokenBucket) Request(tokens float64) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if tokens <= tb.tokens {
		tb.tokens -= tokens
		return true
	}
	return false
}

// WaitForTokens waits until enough tokens are available
func (tb *TokenBucket) WaitForTokens(tokens float64) {
	for !tb.Request(tokens) {
		// Calculate how long to wait based on the refill rate
		tb.mu.Lock()
		missingTokens := tokens - tb.tokens
		waitSeconds := missingTokens / tb.refillRate
		tb.mu.Unlock()
		// Wait for the estimated time, but at least 1ms to avoid busy looping
		waitDuration := time.Duration(math.Max(waitSeconds*float64(time.Second), float64(time.Millisecond)))
		time.Sleep(waitDuration)
	}
}

// Worker function to process download tasks
func worker(id int, jobs <-chan DownloadTask, results chan<- DownloadStatus, tokenBucket *TokenBucket, wg *sync.WaitGroup) {
	defer wg.Done()
	for task := range jobs {
		// Create a control channel for this task
		controlChan := make(chan ControlMessage)
		controlMux.Lock()
		controlChans[task.ID] = controlChan
		controlMux.Unlock()

		// Run the download
		status := downloadFile(task, tokenBucket, controlChan)
		results <- status

		// Clean up the control channel
		controlMux.Lock()
		delete(controlChans, task.ID)
		close(controlChan)
		// Remove from progressMap if canceled
		if status.State == "canceled" {
			progressMux.Lock()
			delete(progressMap, task.FilePath)
			progressMux.Unlock()
		}
		controlMux.Unlock()
	}
}

// downloadFile downloads a file with throttling
func downloadFile(task DownloadTask, tokenBucket *TokenBucket, controlChan chan ControlMessage) DownloadStatus {
	// Ensure Downloads directory exists
	if err := os.MkdirAll(downloadDir+task.FileType, 0755); err != nil {
		return DownloadStatus{URL: task.URL, Error: err}
	}

	// Full path includes Downloads folder
	fullPath := filepath.Join(downloadDir+task.FileType, task.FilePath)

	// Open the file for writing
	file, err := os.Create(fullPath)
	if err != nil {
		return DownloadStatus{URL: task.URL, Error: err}
	}
	defer file.Close()

	// Send an HTTP GET request
	resp, err := http.Get(task.URL)
	if err != nil {
		return DownloadStatus{URL: task.URL, Error: err}
	}
	defer resp.Body.Close()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		return DownloadStatus{URL: task.URL, Error: fmt.Errorf("bad status: %s", resp.Status)}
	}

	// Read the response body in chunks
	buffer := make([]byte, tokenSize)
	totalBytes := resp.ContentLength
	hasTotalBytes := totalBytes != -1
	bytesDownloaded := 0
	accumulatedBytes := 0 // For updating progressMap less frequently
	lastUpdate := time.Now()

	// Initialize status with window tracking
	status := DownloadStatus{
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
				progressMux.Lock()
				progressMap[task.FilePath] = DownloadStatus{
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
				progressMux.Unlock()

				// Wait until resumed or canceled
				for {
					resumeMsg := <-controlChan
					if resumeMsg.TaskID == task.ID {
						if resumeMsg.Action == "resume" {
							progressMux.Lock()
							progressMap[task.FilePath] = DownloadStatus{
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
							progressMux.Unlock()
							status.State = "running"
							break
						} else if resumeMsg.Action == "cancel" {
							// Close file and delete it
							file.Close()
							os.Remove(fullPath)
							// Update progressMap to reflect canceled state
							progressMux.Lock()
							progressMap[task.FilePath] = DownloadStatus{
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
							progressMux.Unlock()
							return DownloadStatus{URL: task.URL, State: "canceled"}
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
				progressMux.Lock()
				progressMap[task.FilePath] = DownloadStatus{
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
				progressMux.Unlock()
				return DownloadStatus{URL: task.URL, State: "canceled"}
			}
		default:
			if status.State != "running" {
				continue
			}

			// Wait for tokens before downloading
			tokenBucket.WaitForTokens(tokenSize)

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
				cutoff := now.Add(-windowDuration)
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
				if accumulatedBytes >= updateIntervalBytes || time.Since(lastUpdate) >= updateIntervalTime {
					progressMux.Lock()
					progressMap[task.FilePath] = DownloadStatus{
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
					progressMux.Unlock()
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
					progressMux.Lock()
					progressMap[task.FilePath] = DownloadStatus{
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
					progressMux.Unlock()
					return DownloadStatus{URL: task.URL, Progress: 100, Speed: 0, State: "completed"}
				}
				return DownloadStatus{URL: task.URL, Error: err}
			}
		}
	}
}

// formatSize converts bytes to human-readable format
func formatSize(bytes int) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// progressBar generates a progress bar string or bytes downloaded if size is unknown
func progressBar(status DownloadStatus) string {
	if !status.HasTotalBytes {
		return fmt.Sprintf("%s downloaded", formatSize(status.BytesDone))
	}
	const width = 20
	completed := status.Progress * width / 100
	bar := strings.Repeat("=", completed) + strings.Repeat("-", width-completed)
	return fmt.Sprintf("[%s] %d%%", bar, status.Progress)
}

// displayProgress updates the terminal with the progress of all downloads
func displayProgress() {
	for {
		fmt.Print("\033[H\033[2J") // Clear the terminal
		progressMux.Lock()
		for filePath, status := range progressMap {
			fmt.Printf("Downloading %s (ID: %d): %s (%d KB/s) ETA: %s State: %s\n", filePath, status.ID, progressBar(status), status.Speed, status.ETA, status.State)
		}
		progressMux.Unlock()
		fmt.Println("\nEnter command (e.g., 'pause 1', 'resume 1', 'cancel 1', or 'exit'):")
		time.Sleep(500 * time.Millisecond)
	}
}

// setFileNames sets filenames based on URLs, handling duplicates
func setFileNames(tasks []DownloadTask) {
	duplicates := make(map[string]int)
	for i, dt := range tasks {
		if dt.FilePath == "" {
			duplicates[dt.URL]++
			temp := strings.Replace(path.Base(dt.URL), "%20", " ", -1)
			if duplicates[dt.URL] == 1 {
				tasks[i].FilePath = temp
			} else {
				temp1 := strings.LastIndex(temp, ".")
				tasks[i].FilePath = fmt.Sprintf("%s (%d)%s", temp[:temp1], duplicates[dt.URL]-1, temp[temp1:])
			}
		}
	}
}

// setFileType determines the file type based on extension
func setFileType(tasks []DownloadTask) {
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

func main() {
	// List of download tasks
	downloadTasks := []DownloadTask{
		{ID: 1, URL: "https://dl.sevilmusics.com/cdn/music/srvrf/Sohrab%20Pakzad%20-%20Dokhtar%20Irooni%20[SevilMusic]%20[Remix].mp3"},

		{ID: 2, URL: "https://soft1.downloadha.com/NarmAfzaar/June2020/Adobe.Photoshop.2020.v21.2.0.225.x64.Portable_www.Downloadha.com_.rar"},
		{ID: 3, URL: "https://dl6.dlhas.ir/behnam/2020/January/Android/Temple-Run-2-1.64.0-Arm64_Downloadha.com_.apk"},
		{ID: 4, URL: "https://dls.musics-fa.com/tagdl/downloads/Homayoun%20Shajarian%20-%20Chera%20Rafti%20(128).mp3"},
	}
	setFileNames(downloadTasks) // TODO should be in a better place! ya do ta ro bezae to ye tabe setType
	setFileType(downloadTasks)  // TODO should be in a better place!

	// Create channels for jobs and results
	jobs := make(chan DownloadTask, len(downloadTasks))
	results := make(chan DownloadStatus, len(downloadTasks))

	// Create a token bucket for throttling (300 KB/s)
	tokenBucket := NewTokenBucket(bandwidthLimit, bandwidthLimit)
	defer tokenBucket.Stop()

	// Start a fixed number of workers
	var wg sync.WaitGroup
	for w := 1; w <= maxConcurrentDownloads; w++ {
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
	go displayProgress()

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
			controlMux.Lock()
			if controlChan, ok := controlChans[taskID]; ok {
				controlChan <- ControlMessage{TaskID: taskID, Action: parts[0]}
				fmt.Printf("%s command sent for task ID %d\n", parts[0], taskID)
			} else {
				fmt.Printf("Task ID %d not found or already completed/canceled\n", taskID)
			}
			controlMux.Unlock()
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
