package main

import (
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
	URL      string
	FilePath string
	FileType string
}

// DownloadStatus represents the status of a download
type DownloadStatus struct {
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

// Constants
const (
	maxConcurrentDownloads = 3                      // Limit to 3 concurrent downloads
	bandwidthLimit         = 1500 * 1024            // 300 KB/s (in bytes)
	tokenSize              = 1024                   // 1 KB per token
	downloadDir            = "Downloads/"           // Folder to save files
	windowDuration         = 1 * time.Second        // Rolling window for speed calculation
	updateIntervalBytes    = 10 * 1024              // Update progressMap every 10 KB
	updateIntervalTime     = 100 * time.Millisecond // Update progressMap every 100ms
	refillInterval         = 10 * time.Millisecond  // Refill token bucket every 10ms
)

// Global variables for progress tracking
var (
	progressMap = make(map[string]DownloadStatus) // Tracks progress of each download
	progressMux = sync.Mutex{}                    // Mutex to protect progressMap
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
		status := downloadFile(task.URL, task.FilePath, task.FileType, tokenBucket)
		results <- status
	}
}

// downloadFile downloads a file with throttling
func downloadFile(url, filePath string, fileType string, tokenBucket *TokenBucket) DownloadStatus {
	// Ensure Downloads directory exists
	if err := os.MkdirAll(downloadDir+fileType, 0755); err != nil {
		return DownloadStatus{URL: url, Error: err}
	}

	// Full path includes Downloads folder
	fullPath := filepath.Join(downloadDir+fileType, filePath)

	// Open the file for writing
	file, err := os.Create(fullPath)
	if err != nil {
		return DownloadStatus{URL: url, Error: err}
	}
	defer file.Close()

	// Send an HTTP GET request
	resp, err := http.Get(url)
	if err != nil {
		return DownloadStatus{URL: url, Error: err}
	}
	defer resp.Body.Close()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		return DownloadStatus{URL: url, Error: fmt.Errorf("bad status: %s", resp.Status)}
	}

	// Read the response body in chunks
	buffer := make([]byte, tokenSize) // 1 KB buffer (matches token size)
	totalBytes := resp.ContentLength
	hasTotalBytes := totalBytes != -1
	bytesDownloaded := 0
	accumulatedBytes := 0 // For updating progressMap less frequently
	lastUpdate := time.Now()

	// Initialize status with window tracking
	status := DownloadStatus{
		URL:           url,
		TotalBytes:    totalBytes,
		HasTotalBytes: hasTotalBytes,
		BytesInWindow: []int{},
		Timestamps:    []time.Time{},
	}

	for {
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
				speed = int(float64(totalBytesInWindow) / windowSeconds / 1024) // Speed in KB/s
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
				progressMap[filePath] = DownloadStatus{
					URL:           url,
					Progress:      progress,
					BytesDone:     bytesDownloaded,
					TotalBytes:    totalBytes,
					HasTotalBytes: hasTotalBytes,
					Speed:         speed,
					ETA:           eta,
					BytesInWindow: status.BytesInWindow,
					Timestamps:    status.Timestamps,
				}
				progressMux.Unlock()
				accumulatedBytes = 0
				lastUpdate = now
			}
		}
		if err != nil {
			if err == io.EOF {
				// Final update to ensure 100% progress is shown TODO ???
				progressMux.Lock()
				progressMap[filePath] = DownloadStatus{
					URL:           url,
					Progress:      100,
					BytesDone:     bytesDownloaded,
					TotalBytes:    totalBytes,
					HasTotalBytes: hasTotalBytes,
					Speed:         0,
					ETA:           "0s",
					BytesInWindow: status.BytesInWindow,
					Timestamps:    status.Timestamps,
				}
				progressMux.Unlock()
				break
			}
			return DownloadStatus{URL: url, Error: err}
		}
	}

	return DownloadStatus{URL: url, Progress: 100, Speed: 0}
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
	const width = 20 // Matches the screenshot
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
			fmt.Printf("Downloading %s: %s (%d KB/s) ETA: %s\n", filePath, progressBar(status), status.Speed, status.ETA)
		}
		progressMux.Unlock()
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
		{URL: "https://dl.sevilmusics.com/cdn/music/srvrf/Sohrab%20Pakzad%20-%20Dokhtar%20Irooni%20[SevilMusic]%20[Remix].mp3"},

		{URL: "https://soft1.downloadha.com/NarmAfzaar/June2020/Adobe.Photoshop.2020.v21.2.0.225.x64.Portable_www.Downloadha.com_.rar"},
		{URL: "https://soft1.downloadha.com/NarmAfzaar/June2020/Adobe.Photoshop.2020.v21.2.0.225.x64.Portable_www.Downloadha.com_.rar"},
		//{URL: "https://dls.musics-fa.com/tagdl/downloads/Homayoun%20Shajarian%20-%20Chera%20Rafti%20(128).mp3"},
	}
	setFileNames(downloadTasks) // TODO should be in a better place! ya do ta ro bezae to ye tabe setType
	setFileType(downloadTasks)  // TODO should be in a better place!

	// Create channels for jobs and results
	jobs := make(chan DownloadTask, len(downloadTasks))
	results := make(chan DownloadStatus, len(downloadTasks))

	// Create a token bucket for throttling (300 KB/s)
	tokenBucket := NewTokenBucket(bandwidthLimit, bandwidthLimit) // maxTokens = bandwidthLimit, refillRate = bandwidthLimit bytes/sec
	defer tokenBucket.Stop()                                      // Ensure the refill goroutine is stopped

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

	// Collect results
	go func() {
		for result := range results {
			if result.Error != nil {
				fmt.Printf("Download failed for %s: %v\n", result.URL, result.Error)
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
