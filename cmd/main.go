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

	"github.com/gammazero/deque"
)

//Changes

// DownloadTask represents a single download task
type DownloadTask struct {
	ID          int
	URL         string
	FilePath    string
	FileType    string
	TokenBucket *TokenBucket
}

// DownloadQueue represents a queue of download tasks with its own limits
type DownloadQueue struct {
	Tasks           []DownloadTask
	SpeedLimit      float64    // Bandwidth limit in bytes/sec
	ConcurrentLimit int        // Max simultaneous downloads
	StartTime       *time.Time // Start time for downloads (nil means no start restriction)
	StopTime        *time.Time // Stop time for downloads (nil means no stop restriction)
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
	totalBandwidthLimit = 500 * 1024             // 500 KB/s total available bandwidth
	tokenSize           = 1024                   // 1 KB per token
	downloadDir         = "Downloads/"           // Folder to save files
	windowDuration      = 1 * time.Second        // Rolling window for speed calculation
	updateIntervalBytes = 10 * 1024              // Update progressMap every 10 KB
	updateIntervalTime  = 100 * time.Millisecond // Update progressMap every 100ms
	refillInterval      = 10 * time.Millisecond  // Refill token bucket every 10ms
)

// Global variables for progress tracking
var (
	progressMap  = make(map[string]DownloadStatus)
	progressMux  = sync.Mutex{}
	controlChans = make(map[int]chan ControlMessage)
	controlMux   = sync.Mutex{}
	stopDisplay  = make(chan struct{}) // Signal to stop displayProgress
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
		tb.mu.Lock()
		missingTokens := tokens - tb.tokens
		waitSeconds := missingTokens / tb.refillRate
		tb.mu.Unlock()
		waitDuration := time.Duration(math.Max(waitSeconds*float64(time.Second), float64(time.Millisecond)))
		time.Sleep(waitDuration)
	}
}

// WorkerPool is a simplified version of the workerpool package
type WorkerPool struct {
	maxWorkers   int
	taskQueue    chan func()
	workerQueue  chan func()
	stopSignal   chan struct{}
	stoppedChan  chan struct{}
	waitingQueue deque.Deque[func()]
	stopLock     sync.Mutex
	stopOnce     sync.Once
	stopped      bool
	wait         bool
}

// New creates a new WorkerPool
func New(maxWorkers int) *WorkerPool {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	pool := &WorkerPool{
		maxWorkers:  maxWorkers,
		taskQueue:   make(chan func()),
		workerQueue: make(chan func()),
		stopSignal:  make(chan struct{}),
		stoppedChan: make(chan struct{}),
	}
	go pool.dispatch()
	return pool
}

// Submit enqueues a function for a worker to execute
func (p *WorkerPool) Submit(task func()) {
	if task != nil {
		p.taskQueue <- task
	}
}

// StopWait stops the worker pool and waits for all queued tasks to complete
func (p *WorkerPool) StopWait() {
	p.stop(true)
}

// dispatch sends tasks to workers
func (p *WorkerPool) dispatch() {
	defer close(p.stoppedChan)
	var workerCount int
	var wg sync.WaitGroup

	for task := range p.taskQueue {
		select {
		case p.workerQueue <- task:
		default:
			if workerCount < p.maxWorkers {
				wg.Add(1)
				go worker(task, p.workerQueue, &wg)
				workerCount++
			} else {
				p.waitingQueue.PushBack(task)
			}
		}
		for p.waitingQueue.Len() > 0 {
			task := p.waitingQueue.PopFront()
			p.workerQueue <- task
		}
	}

	for workerCount > 0 {
		p.workerQueue <- nil
		workerCount--
	}
	wg.Wait()
}

// worker executes tasks
func worker(task func(), workerQueue chan func(), wg *sync.WaitGroup) {
	for task != nil {
		task()
		task = <-workerQueue
	}
	wg.Done()
}

// stop handles pool shutdown
func (p *WorkerPool) stop(wait bool) {
	p.stopOnce.Do(func() {
		close(p.stopSignal)
		p.stopLock.Lock()
		p.stopped = true
		p.stopLock.Unlock()
		p.wait = wait
		close(p.taskQueue)
	})
	<-p.stoppedChan
}

// downloadFile handles the downloading logic
func downloadFile(task DownloadTask, controlChan chan ControlMessage, results chan<- DownloadStatus) {
	if err := os.MkdirAll(downloadDir+task.FileType, 0755); err != nil {
		results <- DownloadStatus{URL: task.URL, Error: err}
		return
	}

	// Use the task's individual token bucket instead of shared one
	tokenBucket := task.TokenBucket

	fullPath := filepath.Join(downloadDir+task.FileType, task.FilePath)
	file, err := os.Create(fullPath)
	if err != nil {
		results <- DownloadStatus{URL: task.URL, Error: err}
		return
	}
	defer file.Close()

	resp, err := http.Get(task.URL)
	if err != nil {
		results <- DownloadStatus{URL: task.URL, Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		results <- DownloadStatus{URL: task.URL, Error: fmt.Errorf("bad status: %s", resp.Status)}
		return
	}

	totalBytes := resp.ContentLength
	hasTotalBytes := totalBytes != -1
	bytesDownloaded := 0
	accumulatedBytes := 0
	lastUpdate := time.Now()
	buffer := make([]byte, tokenSize)
	status := DownloadStatus{
		ID:            task.ID,
		URL:           task.URL,
		TotalBytes:    totalBytes,
		HasTotalBytes: hasTotalBytes,
		BytesInWindow: []int{},
		Timestamps:    []time.Time{},
		State:         "running",
	}

	for {
		select {
		case msg := <-controlChan:
			if msg.TaskID != task.ID {
				continue
			}
			if err := handleControlMessage(&status, task, file, fullPath, bytesDownloaded, totalBytes, hasTotalBytes, controlChan, msg); err != nil {
				results <- DownloadStatus{URL: task.URL, Error: err}
				return
			}
			if status.State == "canceled" {
				results <- status
				return
			}
		default:
			if status.State != "running" {
				continue
			}

			tokenBucket.WaitForTokens(tokenSize)
			n, err := readAndWriteChunk(resp.Body, file, buffer)
			if err != nil {
				if err == io.EOF {
					updateProgressMap(task.FilePath, status, 100, bytesDownloaded, totalBytes, hasTotalBytes, 0, "0s", "completed")
					results <- DownloadStatus{URL: task.URL, Progress: 100, State: "completed"}
					return
				}
				results <- DownloadStatus{URL: task.URL, Error: err}
				return
			}

			bytesDownloaded += n
			accumulatedBytes += n
			updateRollingWindow(&status.BytesInWindow, &status.Timestamps, n, time.Now())
			if accumulatedBytes >= updateIntervalBytes || time.Since(lastUpdate) >= updateIntervalTime {
				progress := calculateProgress(bytesDownloaded, totalBytes, hasTotalBytes)
				if hasTotalBytes && int64(bytesDownloaded) >= totalBytes {
					progress = 100
				}
				speed, eta := calculateSpeedAndETA(status.BytesInWindow, status.Timestamps, bytesDownloaded, totalBytes, hasTotalBytes)
				updateProgressMap(task.FilePath, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, speed, eta, status.State)
				accumulatedBytes = 0
				lastUpdate = time.Now()
			}
		}
	}
}

// Helper functions
func readAndWriteChunk(body io.Reader, file *os.File, buffer []byte) (int, error) {
	n, err := body.Read(buffer)
	if n > 0 {
		if _, err := file.Write(buffer[:n]); err != nil {
			return n, err
		}
	}
	return n, err
}

func calculateProgress(bytesDownloaded int, totalBytes int64, hasTotalBytes bool) int {
	if !hasTotalBytes {
		return 0
	}
	if int64(bytesDownloaded) >= totalBytes {
		return 100
	}
	return int(float64(bytesDownloaded) / float64(totalBytes) * 100)
}

func calculateSpeedAndETA(bytesInWindow []int, timestamps []time.Time, bytesDownloaded int, totalBytes int64, hasTotalBytes bool) (int, string) {
	var totalBytesInWindow int
	for _, b := range bytesInWindow {
		totalBytesInWindow += b
	}
	var windowSeconds float64
	if len(timestamps) > 1 {
		windowSeconds = timestamps[len(timestamps)-1].Sub(timestamps[0]).Seconds()
	} else if len(timestamps) == 1 {
		windowSeconds = time.Since(timestamps[0]).Seconds()
	}
	var speed int
	if windowSeconds > 0 {
		speed = int(float64(totalBytesInWindow) / windowSeconds / 1024)
	}
	var eta string
	if hasTotalBytes && speed > 0 {
		remainingBytes := totalBytes - int64(bytesDownloaded)
		if remainingBytes <= 0 {
			eta = "0s"
		} else {
			etaSeconds := float64(remainingBytes) / float64(speed*1024)
			eta = time.Duration(etaSeconds * float64(time.Second)).Truncate(time.Second).String()
		}
	} else {
		eta = "N/A"
	}
	return speed, eta
}

func updateRollingWindow(bytesInWindow *[]int, timestamps *[]time.Time, n int, now time.Time) {
	*bytesInWindow = append(*bytesInWindow, n)
	*timestamps = append(*timestamps, now)
	cutoff := now.Add(-windowDuration)
	newBytes := []int{}
	newTimes := []time.Time{}
	for i, ts := range *timestamps {
		if ts.After(cutoff) {
			newBytes = append(newBytes, (*bytesInWindow)[i])
			newTimes = append(newTimes, ts)
		}
	}
	*bytesInWindow = newBytes
	*timestamps = newTimes
}

func newDownloadStatus(task DownloadTask, status *DownloadStatus, progress int, bytesDownloaded int, totalBytes int64, hasTotalBytes bool, speed int, eta string, state string) DownloadStatus {
	return DownloadStatus{
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
		State:         state,
	}
}

func handleControlMessage(status *DownloadStatus, task DownloadTask, file *os.File, fullPath string, bytesDownloaded int, totalBytes int64, hasTotalBytes bool, controlChan chan ControlMessage, msg ControlMessage) error {
	progress := calculateProgress(bytesDownloaded, totalBytes, hasTotalBytes)
	speed, eta := calculateSpeedAndETA(status.BytesInWindow, status.Timestamps, bytesDownloaded, totalBytes, hasTotalBytes)

	switch msg.Action {
	case "pause":
		progressMux.Lock()
		progressMap[task.FilePath] = newDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, speed, eta, "paused")
		progressMux.Unlock()
		for {
			resumeMsg := <-controlChan
			if resumeMsg.TaskID == task.ID {
				if resumeMsg.Action == "resume" {
					progressMux.Lock()
					progressMap[task.FilePath] = newDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, speed, eta, "running")
					progressMux.Unlock()
					*status = newDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, speed, eta, "running")
					return nil
				} else if resumeMsg.Action == "cancel" {
					file.Close()
					if err := os.Remove(fullPath); err != nil {
						return err
					}
					progressMux.Lock()
					progressMap[task.FilePath] = newDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, 0, "N/A", "canceled")
					progressMux.Unlock()
					*status = newDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, 0, "N/A", "canceled")
					return nil
				}
			}
		}
	case "cancel":
		file.Close()
		if err := os.Remove(fullPath); err != nil {
			return err
		}
		progressMux.Lock()
		progressMap[task.FilePath] = newDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, 0, "N/A", "canceled")
		progressMux.Unlock()
		*status = newDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, 0, "N/A", "canceled")
		return nil
	}
	return nil
}

func updateProgressMap(filePath string, status DownloadStatus, progress int, bytesDownloaded int, totalBytes int64, hasTotalBytes bool, speed int, eta string, state string) {
	progressMux.Lock()
	progressMap[filePath] = DownloadStatus{
		ID:            status.ID,
		URL:           status.URL,
		Progress:      progress,
		BytesDone:     bytesDownloaded,
		TotalBytes:    totalBytes,
		HasTotalBytes: hasTotalBytes,
		Speed:         speed,
		ETA:           eta,
		BytesInWindow: status.BytesInWindow,
		Timestamps:    status.Timestamps,
		State:         state,
	}
	progressMux.Unlock()
}

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

func progressBar(status DownloadStatus) string {
	if !status.HasTotalBytes {
		return fmt.Sprintf("%s downloaded", formatSize(status.BytesDone))
	}
	const width = 20
	completed := status.Progress * width / 100
	bar := strings.Repeat("=", completed) + strings.Repeat("-", width-completed)
	return fmt.Sprintf("[%s] %d%%", bar, status.Progress)
}

func displayProgress(wg *sync.WaitGroup) {
	defer wg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stopDisplay:
			fmt.Println("\nProgress display stopped.")
			return
		case <-ticker.C:
			fmt.Print("\033[H\033[2J") // Clear terminal
			progressMux.Lock()
			if len(progressMap) == 0 {
				fmt.Println("No active downloads.")
			} else {
				for filePath, status := range progressMap {
					fmt.Printf("Downloading %s (ID: %d): %s (%d KB/s) ETA: %s State: %s\n",
						filePath, status.ID, progressBar(status), status.Speed, status.ETA, status.State)
				}
			}
			progressMux.Unlock()
			fmt.Println("\nEnter command (e.g., 'pause 1', 'resume 1', 'cancel 1', or 'exit'):")
		}
	}
}

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

// processQueue updated to handle nil StartTime/StopTime
func processQueue(queue DownloadQueue, pool *WorkerPool, results chan<- DownloadStatus) {
	queuePool := New(queue.ConcurrentLimit)

	// Calculate bandwidth per download in this queue
	bandwidthPerDownload := queue.SpeedLimit / float64(queue.ConcurrentLimit)

	// Assign token buckets to each task in the queue
	for i := range queue.Tasks {
		queue.Tasks[i].TokenBucket = NewTokenBucket(bandwidthPerDownload, bandwidthPerDownload)
	}

	// Time check loop (only if either StartTime or StopTime is set)
	if queue.StartTime != nil || queue.StopTime != nil {
		go func() {
			for {
				now := time.Now()
				withinTimeWindow := true

				// Check StartTime if set
				if queue.StartTime != nil {
					startToday := time.Date(now.Year(), now.Month(), now.Day(), queue.StartTime.Hour(), queue.StartTime.Minute(), 0, 0, time.Local)
					if now.Before(startToday) {
						withinTimeWindow = false
					}
				}

				// Check StopTime if set
				if queue.StopTime != nil {
					stopToday := time.Date(now.Year(), now.Month(), now.Day(), queue.StopTime.Hour(), queue.StopTime.Minute(), 0, 0, time.Local)
					if now.After(stopToday) {
						withinTimeWindow = false
					}
				}

				for _, task := range queue.Tasks {
					controlMux.Lock()
					if controlChan, ok := controlChans[task.ID]; ok {
						if withinTimeWindow {
							if status, exists := progressMap[task.FilePath]; exists && status.State == "paused" && status.Error == nil {
								controlChan <- ControlMessage{TaskID: task.ID, Action: "resume"}
								fmt.Printf("Resuming task %d due to time window\n", task.ID)
							}
						} else {
							if status, exists := progressMap[task.FilePath]; exists && status.State == "running" {
								controlChan <- ControlMessage{TaskID: task.ID, Action: "pause"}
								fmt.Printf("Pausing task %d due to time window\n", task.ID)
							}
						}
					}
					controlMux.Unlock()
				}

				// Check every 10 seconds for testing (adjust as needed)
				time.Sleep(10 * time.Second)
			}
		}()
	}

	for _, task := range queue.Tasks {
		controlChan := make(chan ControlMessage)
		controlMux.Lock()
		controlChans[task.ID] = controlChan
		controlMux.Unlock()

		queuePool.Submit(func() {
			downloadFile(task, controlChan, results)
			controlMux.Lock()
			delete(controlChans, task.ID)
			close(controlChan)
			if status, ok := progressMap[task.FilePath]; ok && (status.State == "completed" || status.State == "canceled") {
				progressMux.Lock()
				delete(progressMap, task.FilePath)
				progressMux.Unlock()
			}
			controlMux.Unlock()
			task.TokenBucket.Stop()
		})
	}

	queuePool.StopWait()
}

func main() {
	now := time.Now()
	startTime1 := now                     // Start now
	stopTime1 := now.Add(4 * time.Second) // Stop in 2 hours

	// Define multiple download queues with optional time limits
	downloadQueues := []DownloadQueue{
		{
			Tasks: []DownloadTask{
				{ID: 1, URL: "https://dl.sevilmusics.com/cdn/music/srvrf/Sohrab%20Pakzad%20-%20Dokhtar%20Irooni%20[SevilMusic]%20[Remix].mp3"},
				{ID: 2, URL: "https://dls.musics-fa.com/tagdl/downloads/Homayoun%20Shajarian%20-%20Chera%20Rafti%20(128).mp3"},
			},
			SpeedLimit:      float64(totalBandwidthLimit) * 0.6, // 300 KB/s
			ConcurrentLimit: 1,
			StartTime:       &startTime1, // Start now
			StopTime:        &stopTime1,  // Stop in 2 hours
		},
		{
			Tasks: []DownloadTask{
				{ID: 3, URL: "https://soft1.downloadha.com/NarmAfzaar/June2020/Adobe.Photoshop.2020.v21.2.0.225.x64.Portable_www.Downloadha.com_.rar"},
				{ID: 4, URL: "https://dl6.dlhas.ir/behnam/2020/January/Android/Temple-Run-2-1.64.0-Arm64_Downloadha.com_.apk"},
			},
			SpeedLimit:      float64(totalBandwidthLimit) * 0.4, // 200 KB/s
			ConcurrentLimit: 1,
			StartTime:       nil, // No start time (infinite)
			StopTime:        nil, // No stop time (infinite)
		},
	}

	// Set file names and types for all tasks in all queues
	for i := range downloadQueues {
		setFileNames(downloadQueues[i].Tasks)
		setFileType(downloadQueues[i].Tasks)
	}

	results := make(chan DownloadStatus, 10)
	pool := New(2)

	var wg sync.WaitGroup

	wg.Add(1)
	go displayProgress(&wg)

	// Process each queue
	for _, queue := range downloadQueues {
		pool.Submit(func() {
			processQueue(queue, pool, results)
		})
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			input := strings.TrimSpace(scanner.Text())
			if input == "exit" {
				fmt.Println("Exiting...")
				pool.StopWait()
				close(stopDisplay)
				return
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
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		totalTasks := 0
		for _, q := range downloadQueues {
			totalTasks += len(q.Tasks)
		}
		count := 0
		for result := range results {
			if result.Error != nil {
				fmt.Printf("Download failed for %s: %v\n", result.URL, result.Error)
			} else if result.State == "canceled" {
				fmt.Printf("Download canceled for %s\n", result.URL)
			} else {
				fmt.Printf("Download completed for %s\n", result.URL)
			}
			count++
			if count == totalTasks {
				close(stopDisplay)
			}
		}
	}()

	pool.StopWait()
	close(results)
	wg.Wait()
	fmt.Println("All downloads processed.")
}
