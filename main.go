package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// DownloadTask represents a single download task
type DownloadTask struct {
	URL      string
	FilePath string
}

// DownloadStatus represents the status of a download
type DownloadStatus struct {
	URL      string
	Progress int
	Speed    int
	Error    error
}

// Constants
const (
	maxConcurrentDownloads = 3           // Limit to 3 concurrent downloads
	bandwidthLimit         = 5000 * 1024 // 300 KB/s
	tokenSize              = 1024        // 1 KB per token
)

// Global variables for progress tracking
var (
	progressMap = make(map[string]DownloadStatus) // Tracks progress of each download
	progressMux = sync.Mutex{}                    // Mutex to protect progressMap
)

// Worker function to process download tasks
func worker(id int, jobs <-chan DownloadTask, results chan<- DownloadStatus, tokenBucket <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	for task := range jobs {
		status := downloadFile(task.URL, task.FilePath, tokenBucket)
		results <- status
	}
}

// downloadFile downloads a file with throttling
func downloadFile(url, filePath string, tokenBucket <-chan struct{}) DownloadStatus {
	// Open the file for writing
	file, err := os.Create(filePath)
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

	// Read the response body in chunks
	buffer := make([]byte, tokenSize) // 1 KB buffer (matches token size)
	totalBytes := resp.ContentLength
	bytesDownloaded := 0
	startTime := time.Now()

	for {
		<-tokenBucket // Acquire a token before downloading
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			file.Write(buffer[:n])
			bytesDownloaded += n
			// Calculate progress and speed
			progress := int(float64(bytesDownloaded) / float64(totalBytes) * 100)
			elapsed := time.Since(startTime).Seconds()
			speed := int(float64(bytesDownloaded) / elapsed / 1024) // Speed in KB/s
			// Update progress in the shared map
			progressMux.Lock()
			progressMap[filePath] = DownloadStatus{URL: url, Progress: progress, Speed: speed}
			progressMux.Unlock()
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return DownloadStatus{URL: url, Error: err}
		}
	}

	return DownloadStatus{URL: url, Progress: 100, Speed: 0}
}

// progressBar generates a progress bar string
func progressBar(progress int) string {
	const width = 50
	completed := progress * width / 100
	bar := ""
	for i := 0; i < width; i++ {
		if i < completed {
			bar += "="
		} else {
			bar += " "
		}
	}
	return fmt.Sprintf("[%s]", bar)
}

// displayProgress updates the terminal with the progress of all downloads
func displayProgress() {
	for {
		// Clear the terminal (optional, for better visualization)
		fmt.Print("\033[H\033[2J")

		// Print the progress of all downloads
		progressMux.Lock()
		for filePath, status := range progressMap {
			fmt.Printf("Downloading %s: %s %d%% (%d KB/s)\n", filePath, progressBar(status.Progress), status.Progress, status.Speed)
		}
		progressMux.Unlock()

		// Wait for a short time before updating again
		time.Sleep(500 * time.Millisecond)
	}
}

func main() {
	// List of download tasks
	downloadTasks := []DownloadTask{
		{URL: "https://dls.musics-fa.com/tagdl/downloads/Homayoun%20Shajarian%20-%20Chera%20Rafti%20(320).mp3", FilePath: "1.mp3"},
		{URL: "https://dls.musics-fa.com/tagdl/downloads/Homayoun%20Shajarian%20-%20Chera%20Rafti%20(320).mp3", FilePath: "file2.zip"},
		{URL: "https://dls.musics-fa.com/tagdl/downloads/Homayoun%20Shajarian%20-%20Chera%20Rafti%20(320).mp3", FilePath: "file3.zip"},
		{URL: "https://dls.musics-fa.com/tagdl/downloads/Homayoun%20Shajarian%20-%20Chera%20Rafti%20(320).mp3", FilePath: "file4.mp3"},
	}

	// Create channels for jobs and results
	jobs := make(chan DownloadTask, len(downloadTasks))
	results := make(chan DownloadStatus, len(downloadTasks))

	// Create a token bucket for throttling
	tokenBucket := make(chan struct{}, bandwidthLimit/tokenSize)

	// Replenish tokens at a fixed rate (e.g., 300 KB/s)
	go func() {
		ticker := time.NewTicker(time.Second)
		for range ticker.C {
			for i := 0; i < bandwidthLimit/tokenSize; i++ {
				tokenBucket <- struct{}{}
			}
		}
	}()

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
	close(results)
}
