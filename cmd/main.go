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
	"strconv"
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
	QueueID     int // Add QueueID to identify which queue this task belongs to
}

// DownloadQueue represents a queue of download tasks with its own limits
type DownloadQueue struct {
	Tasks            []DownloadTask
	SpeedLimit       float64    // Bandwidth limit in bytes/sec
	ConcurrentLimit  int        // Max simultaneous downloads
	StartTime        *time.Time // Start time for downloads (nil means no start restriction)
	StopTime         *time.Time // Stop time for downloads (nil means no stop restriction)
	MaxRetries       int        // Maximum number of retries for failed downloads
	ID               int        // Unique ID for each queue
	DownloadLocation string     // Custom download location for this queue
}

// DownloadStatus represents the status of a download
type DownloadStatus struct {
	ID            int
	URL           string
	Progress      int
	BytesDone     int
	TotalBytes    int64
	HasTotalBytes bool
	State         string
	Speed         int
	ETA           string
	BytesInWindow []int
	Timestamps    []time.Time
	Error         error
	RetriesLeft   int // Track remaining retries
}

// ControlMessage represents a control command
type ControlMessage struct {
	TaskID int
	Action string // "pause", "resume", or "cancel"
}

// Constants
const (
	totalBandwidthLimit = 500 * 1024   // 500 KB/s total available bandwidth
	tokenSize           = 1024         // 1 KB per token
	subDirDownloads     = "Downloads/" // Subdirectory name

	windowDuration      = 1 * time.Second        // Rolling window for speed calculation
	updateIntervalBytes = 10 * 1024              // Update progressMap every 10 KB
	updateIntervalTime  = 100 * time.Millisecond // Update progressMap every 100ms
	refillInterval      = 10 * time.Millisecond  // Refill token bucket every 10ms
	retryDelay          = 5 * time.Second        // Delay between retries
)

// Global variables for progress tracking
var (
	progressMap    = make(map[string]DownloadStatus)
	progressMux    = sync.Mutex{}
	controlChans   = make(map[int]chan ControlMessage)
	controlMux     = sync.Mutex{}
	stopDisplay    = make(chan struct{})         // Signal to stop displayProgress
	activeTasks    = sync.Map{}                  // Map[queueID][taskID]bool to track active downloads
	queues         = make(map[int]DownloadQueue) // Store queues by ID for O(1) lookup
	bandwidthChans = make(map[int]chan struct{}) // Channels to trigger bandwidth redistribution per queue
	bandwidthMux   = sync.Mutex{}
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

// NewWorkerPool New creates a new Worker Pool
func NewWorkerPool(maxWorkers int) *WorkerPool {
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

func downloadFile(task DownloadTask, controlChan chan ControlMessage, results chan<- DownloadStatus, maxRetries int, queue DownloadQueue) {
	retriesLeft := maxRetries
	for {
		// Notify start of download
		activeTasks.Store(fmt.Sprintf("%d:%d", queue.ID, task.ID), true)
		triggerBandwidthRedistribution(queue.ID)

		status := DownloadStatus{
			ID:          task.ID,
			URL:         task.URL,
			State:       "running",
			RetriesLeft: retriesLeft,
		}
		err := tryDownload(task, controlChan, results, &status)
		if err != nil && status.State == "reset" {
			// Reset requested: clean up and restart the download
			fmt.Printf("Resetting download for task %d\n", task.ID)
			continue // Restart the loop to reset the download
		}
		if err != nil {
			if err == io.EOF || status.State == "completed" || status.State == "canceled" {
				activeTasks.Delete(fmt.Sprintf("%d:%d", queue.ID, task.ID))
				triggerBandwidthRedistribution(queue.ID)
				return // Success or intentional stop
			}
			retriesLeft--
			status.Error = err
			status.State = "failed"
			updateProgressMap(task.FilePath, status, status.Progress, status.BytesDone, status.TotalBytes, status.HasTotalBytes, status.Speed, status.ETA, status.State)
			if retriesLeft >= 0 {
				fmt.Printf("Download failed for %s: %v. Retries left: %d. Retrying in %v...\n", task.URL, err, retriesLeft, retryDelay)
				time.Sleep(retryDelay)
				continue
			}
			// After maxRetries, explicitly cancel the download
			fmt.Printf("Max retries reached for %s. Canceling download.\n", task.URL)
			controlChan <- ControlMessage{TaskID: task.ID, Action: "cancel"}
			status.State = "canceled"
			status.Error = fmt.Errorf("download canceled after %d failed attempts: %v", maxRetries+1, err)
			results <- status
			activeTasks.Delete(fmt.Sprintf("%d:%d", queue.ID, task.ID))
			triggerBandwidthRedistribution(queue.ID)
			return
		}
		activeTasks.Delete(fmt.Sprintf("%d:%d", queue.ID, task.ID))
		triggerBandwidthRedistribution(queue.ID)
		return // Success
	}
}

func tryDownload(task DownloadTask, controlChan chan ControlMessage, results chan<- DownloadStatus, status *DownloadStatus) error {
	fullPath, err := setupDownloadFile(task, queues[task.QueueID])
	if err != nil {
		return err
	}

	file, resp, err := initiateDownload(fullPath, task.URL)
	if err != nil {
		return err
	}
	// Track if file is closed to avoid double-closing
	fileClosed := false
	defer func() {
		if !fileClosed {
			if err := file.Close(); err != nil {
				fmt.Printf("Error closing file %s: %v\n", fullPath, err)
			}
		}
		resp.Body.Close()
	}()

	return processDownload(task, file, resp, controlChan, results, status, &fileClosed, fullPath)
}

func setupDownloadFile(task DownloadTask, queue DownloadQueue) (string, error) {
	baseDir := queue.DownloadLocation
	if baseDir == "" {
		baseDir = "." // Default to current directory if not specified
	}
	fullDir := filepath.Join(baseDir, subDirDownloads, task.FileType)
	if err := os.MkdirAll(fullDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create directory %s: %v", fullDir, err)
	}
	return filepath.Join(fullDir, task.FilePath), nil
}

func initiateDownload(fullPath, url string) (*os.File, *http.Response, error) {
	file, err := os.Create(fullPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create file %s: %v", fullPath, err)
	}
	resp, err := http.Get(url)
	if err != nil {
		file.Close()
		return nil, nil, fmt.Errorf("failed to initiate download from %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		file.Close()
		resp.Body.Close()
		return nil, nil, fmt.Errorf("bad status for %s: %s", url, resp.Status)
	}
	return file, resp, nil
}

func processDownload(task DownloadTask, file *os.File, resp *http.Response, controlChan chan ControlMessage, results chan<- DownloadStatus, status *DownloadStatus, fileClosed *bool, fullPath string) error {
	status.TotalBytes = resp.ContentLength
	status.HasTotalBytes = status.TotalBytes != -1
	status.BytesInWindow = []int{}
	status.Timestamps = []time.Time{}

	bytesDownloaded := 0
	accumulatedBytes := 0
	lastUpdate := time.Now()
	buffer := make([]byte, tokenSize)

	for {
		select {
		case msg := <-controlChan:
			if msg.TaskID == task.ID {
				if msg.Action == "reset" {
					// Handle reset: clean up and signal to restart
					*fileClosed = true
					if err := file.Close(); err != nil {
						fmt.Printf("Error closing file %s during reset: %v\n", fullPath, err)
					}
					if err := safeRemoveFile(fullPath); err != nil {
						fmt.Printf("Error removing file %s during reset: %v\n", fullPath, err)
					}
					status.State = "reset"
					status.Progress = 0
					status.BytesDone = 0
					status.BytesInWindow = []int{}
					status.Timestamps = []time.Time{}
					status.Speed = 0
					status.ETA = "N/A"
					updateProgressMap(task.FilePath, *status, 0, 0, status.TotalBytes, status.HasTotalBytes, 0, "N/A", "reset")
					return fmt.Errorf("reset requested")
				}
				if err := handleControlMessage(status, task, file, fullPath, bytesDownloaded, status.TotalBytes, status.HasTotalBytes, controlChan, msg); err != nil {
					return err
				}
				if status.State == "canceled" {
					*fileClosed = true
					results <- *status
					return nil
				}
			}
		default:
			if status.State != "running" {
				continue
			}
			if err := downloadChunk(task, file, resp.Body, buffer, status, &bytesDownloaded, &accumulatedBytes, &lastUpdate); err != nil {
				if err == io.EOF {
					updateProgressMap(task.FilePath, *status, 100, bytesDownloaded, status.TotalBytes, status.HasTotalBytes, 0, "0s", "completed")
					status.State = "completed"
					results <- *status
					*fileClosed = true
					return nil
				}
				return fmt.Errorf("download chunk failed: %v", err)
			}
		}
	}
}

func safeRemoveFile(filePath string) error {
	//TODO max file remove retries 5
	for i := 0; i < 5; i++ {
		err := os.Remove(filePath)
		if err == nil {
			return nil
		}
		// Check if the error is due to the file being in use
		if strings.Contains(err.Error(), "being used by another process") {
			fmt.Printf("File %s in use, retrying removal (%d/%d)...\n", filePath, i+1, 5)
			time.Sleep(time.Millisecond * 500)
			continue
		}
		return fmt.Errorf("failed to remove file %s: %v", filePath, err)
	}
	return fmt.Errorf("failed to remove file %s after %d attempts: file in use", filePath, time.Millisecond*500)
}

func downloadChunk(task DownloadTask, file *os.File, body io.Reader, buffer []byte, status *DownloadStatus, bytesDownloaded, accumulatedBytes *int, lastUpdate *time.Time) error {
	task.TokenBucket.WaitForTokens(tokenSize)
	n, err := readAndWriteChunk(body, file, buffer)
	if err != nil {
		return err
	}
	*bytesDownloaded += n
	*accumulatedBytes += n
	updateRollingWindow(&status.BytesInWindow, &status.Timestamps, n, time.Now())
	if *accumulatedBytes >= updateIntervalBytes || time.Since(*lastUpdate) >= updateIntervalTime {
		updateProgress(status, task.FilePath, *bytesDownloaded, status.TotalBytes, status.HasTotalBytes)
		*accumulatedBytes = 0
		*lastUpdate = time.Now()
	}
	return nil
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

// Updated handleControlMessage to match the parameters
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
						return fmt.Errorf("failed to remove file %s during cancel: %v", fullPath, err)
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
			return fmt.Errorf("failed to remove file %s during cancel: %v", fullPath, err)
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
			//fmt.Print("\033[H\033[2J") // Clear terminal
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

func startTimeCheck(queue DownloadQueue) {
	if queue.StartTime != nil || queue.StopTime != nil {
		go func() {
			for {
				checkQueueTimeWindow(queue)
				time.Sleep(10 * time.Second)
			}
		}()
	}
}

// processQueue updated to handle nil StartTime/StopTime
func processQueue(queue DownloadQueue, pool *WorkerPool, results chan<- DownloadStatus) {
	queuePool, err := setupQueuePool(queue)
	if err != nil {
		fmt.Printf("Failed to setup queue pool: %v\n", err)
		return
	}
	startTimeCheck(queue)
	submitQueueTasks(queue, queuePool, results)
	queuePool.StopWait()
}

func setupQueuePool(queue DownloadQueue) (*WorkerPool, error) {
	if len(queue.Tasks) == 0 {
		return nil, fmt.Errorf("queue has no tasks")
	}
	// Initial bandwidth split based on ConcurrentLimit
	initialBandwidth := queue.SpeedLimit / float64(queue.ConcurrentLimit)
	for i := range queue.Tasks {
		queue.Tasks[i].QueueID = queue.ID
		queue.Tasks[i].TokenBucket = NewTokenBucket(initialBandwidth, initialBandwidth)
	}
	queues[queue.ID] = queue // Store queue for later reference

	// Start bandwidth redistribution goroutine for this queue
	bandwidthChans[queue.ID] = make(chan struct{}, 1)
	go manageBandwidthRedistribution(queue.ID)

	// Trigger initial redistribution after all tasks are set up
	time.Sleep(100 * time.Millisecond) // Allow tasks to register
	triggerBandwidthRedistribution(queue.ID)

	return NewWorkerPool(queue.ConcurrentLimit), nil
}

func triggerBandwidthRedistribution(queueID int) {
	bandwidthMux.Lock()
	if ch, exists := bandwidthChans[queueID]; exists {
		select {
		case ch <- struct{}{}:
			// Successfully triggered redistribution
		default:
			// Channel already has a pending request
		}
	}
	bandwidthMux.Unlock()
}

func manageBandwidthRedistribution(queueID int) {
	for range bandwidthChans[queueID] {
		if queue, exists := queues[queueID]; exists {
			redistributeBandwidth(queue)
		}
	}
}

func updateQueueSpeedLimit(queueID int, newSpeedLimit float64) {
	if queue, exists := queues[queueID]; exists {
		queue.SpeedLimit = newSpeedLimit
		queues[queueID] = queue // Update the global queues map
		fmt.Printf("Updated speed limit for Queue %d to %f KB/s\n", queueID, newSpeedLimit/1024)
		triggerBandwidthRedistribution(queueID)
	} else {
		fmt.Printf("Queue %d not found\n", queueID)
	}
}

func redistributeBandwidth(queue DownloadQueue) {
	var activeCount int
	activeTasks.Range(func(key, value interface{}) bool {
		if k, ok := key.(string); ok {
			parts := strings.Split(k, ":")
			if len(parts) == 2 && parts[0] == fmt.Sprint(queue.ID) {
				activeCount++
			}
		}
		return true
	})

	if activeCount == 0 {
		return // No active tasks, no need to redistribute
	}

	newBandwidthPerTask := queue.SpeedLimit / float64(activeCount)
	activeTasks.Range(func(key, value interface{}) bool {
		if k, ok := key.(string); ok {
			parts := strings.Split(k, ":")
			if len(parts) == 2 && parts[0] == fmt.Sprint(queue.ID) {
				taskID, _ := strconv.Atoi(parts[1])
				for _, task := range queue.Tasks {
					if task.ID == taskID && task.TokenBucket != nil {
						task.TokenBucket.mu.Lock()
						task.TokenBucket.maxTokens = newBandwidthPerTask
						task.TokenBucket.tokens = math.Min(task.TokenBucket.tokens, newBandwidthPerTask) // Avoid overfilling
						task.TokenBucket.refillRate = newBandwidthPerTask
						task.TokenBucket.mu.Unlock()
						fmt.Printf("Task %d updated to %f KB/s (active tasks: %d)\n", taskID, newBandwidthPerTask/1024, activeCount) // Debug output
					}
				}
			}
		}
		return true
	})
}

func checkQueueTimeWindow(queue DownloadQueue) {
	now := time.Now()
	withinTimeWindow := true
	if queue.StartTime != nil {
		startToday := time.Date(queue.StartTime.Year(), queue.StartTime.Month(), queue.StartTime.Day(), queue.StartTime.Hour(), queue.StartTime.Minute(), 0, 0, time.Local)

		if now.Before(startToday) {
			fmt.Printf("enterd for start")
			withinTimeWindow = false
		}
	}
	if queue.StopTime != nil {
		stopToday := time.Date(queue.StopTime.Year(), queue.StopTime.Month(), queue.StopTime.Day(), queue.StopTime.Hour(), queue.StopTime.Minute(), 0, 0, time.Local)
		if now.After(stopToday) {
			fmt.Printf("enterd for stop")
			withinTimeWindow = false
		}
	}
	controlQueueTasks(queue, withinTimeWindow)
}

func controlQueueTasks(queue DownloadQueue, withinTimeWindow bool) {
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
}

func submitQueueTasks(queue DownloadQueue, queuePool *WorkerPool, results chan<- DownloadStatus) {
	for _, task := range queue.Tasks {
		controlChan := make(chan ControlMessage, 1) // Buffered to avoid blocking
		controlMux.Lock()
		controlChans[task.ID] = controlChan
		controlMux.Unlock()
		queuePool.Submit(func() {
			downloadFile(task, controlChan, results, queue.MaxRetries, queue)
			cleanupTask(task)
		})
	}
}

func cleanupTask(task DownloadTask) {
	controlMux.Lock()
	defer controlMux.Unlock()

	controlChan, exists := controlChans[task.ID]
	if exists {
		delete(controlChans, task.ID)
		select {
		case <-controlChan:
			// Channel is already closed, do nothing
		default:
			close(controlChan)
		}
	}

	progressMux.Lock()
	if status, ok := progressMap[task.FilePath]; ok && (status.State == "completed" || status.State == "canceled") {
		delete(progressMap, task.FilePath)
	}
	progressMux.Unlock()

	if task.TokenBucket != nil {
		task.TokenBucket.Stop()
	}

	// Notify end of download
	activeTasks.Delete(fmt.Sprintf("%d:%d", task.QueueID, task.ID))
	if queue, exists := queues[task.QueueID]; exists {
		redistributeBandwidth(queue)
	}
}

// ---- Helper Functions ----
func updateProgress(status *DownloadStatus, filePath string, bytesDownloaded int, totalBytes int64, hasTotalBytes bool) {
	progress := calculateProgress(bytesDownloaded, totalBytes, hasTotalBytes)
	speed, eta := calculateSpeedAndETA(status.BytesInWindow, status.Timestamps, bytesDownloaded, totalBytes, hasTotalBytes)
	updateProgressMap(filePath, *status, progress, bytesDownloaded, totalBytes, hasTotalBytes, speed, eta, status.State)
}

// ---- Main and Setup Functions ----
func setupDownloadQueues() []DownloadQueue {
	now := time.Now()
	startTime1 := now
	stopTime1 := now.Add(2 * time.Hour)
	queues := []DownloadQueue{
		{
			Tasks: []DownloadTask{
				{ID: 1, URL: "https://dl.sevilmusics.com/cdn/music/srvrf/Sohrab%20Pakzad%20-%20Dokhtar%20Irooni%20[SevilMusic]%20[Remix].mp3"},
				{ID: 2, URL: "https://dls.musics-fa.com/tagdl/downloads/Homayoun%20Shajarian%20-%20Chera%20Rafti%20(128).mp3"},
				{ID: 3, URL: "https://soft1.downloadha.com/NarmAfzaar/June2020/Adobe.Photoshop.2020.v21.2.0.225.x64.Portable_www.Downloadha.com_.rar"}, // Additional link for testing
				//{ID: 4, URL: "https://dl6.dlhas.ir/behnam/2020/January/Android/Temple-Run-2-1.64.0-Arm64_Downloadha.com_.apk"},
				//{ID: 5, URL: "https://example.com/file3.mp3"},
			},
			SpeedLimit:       float64(totalBandwidthLimit), // 500 KB/s
			ConcurrentLimit:  2,
			StartTime:        &startTime1,
			StopTime:         &stopTime1,
			MaxRetries:       3,
			ID:               1,
			DownloadLocation: "C:/CustomDownloads", // Example custom location
		},
		/*{
		    Tasks: []DownloadTask{
		        {ID: 6, URL: "https://soft1.downloadha.com/NarmAfzaar/June2020/Adobe.Photoshop.2020.v21.2.0.225.x64.Portable_www.Downloadha.com_.rar"},
		        {ID: 7, URL: "https://dl6.dlhas.ir/behnam/2020/January/Android/Temple-Run-2-1.64.0-Arm64_Downloadha.com_.apk"},
		    },
		    SpeedLimit:      float64(totalBandwidthLimit) * 0.4, // 200 KB/s
		    ConcurrentLimit: 2,
		    StartTime:       nil,
		    StopTime:        nil,
		    MaxRetries:      2,
		    ID:              2,
		    DownloadLocation: "D:/AnotherLocation", // Another example custom location
		},*/
	}
	for i := range queues {
		setFileNames(queues[i].Tasks)
		setFileType(queues[i].Tasks)
	}
	return queues
}

func startProgressDisplay(wg *sync.WaitGroup) {
	wg.Add(1)
	go displayProgress(wg)
}

func handleUserInput(wg *sync.WaitGroup, pool *WorkerPool) {
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

func processCommand(input string, pool *WorkerPool) bool {
	input = strings.TrimSpace(input)
	if input == "exit" {
		fmt.Println("Exiting...")
		pool.StopWait()
		close(stopDisplay)
		return false
	}
	parts := strings.Split(input, " ")
	if len(parts) < 2 {
		fmt.Println("Invalid command. Use 'pause ID', 'resume ID', 'cancel ID', 'reset ID', or 'speed QUEUE_ID KB_PER_SEC' (e.g., 'pause 1' or 'speed 1 600')")
		return true
	}

	action := parts[0]
	if action == "speed" {
		if len(parts) != 3 {
			fmt.Println("Invalid speed command. Use 'speed QUEUE_ID KB_PER_SEC' (e.g., 'speed 1 600')")
			return true
		}
		var queueID int
		if _, err := fmt.Sscanf(parts[1], "%d", &queueID); err != nil {
			fmt.Println("Invalid queue ID. Must be a number.")
			return true
		}
		var speedKB int
		if _, err := fmt.Sscanf(parts[2], "%d", &speedKB); err != nil {
			fmt.Println("Invalid speed. Must be a number (KB/s).")
			return true
		}
		updateQueueSpeedLimit(queueID, float64(speedKB*1024))
		return true
	}

	if len(parts) != 2 {
		fmt.Println("Invalid command. Use 'pause ID', 'resume ID', 'cancel ID', 'reset ID', or 'speed QUEUE_ID KB_PER_SEC' (e.g., 'pause 1' or 'speed 1 600')")
		return true
	}
	var taskID int
	if _, err := fmt.Sscanf(parts[1], "%d", &taskID); err != nil {
		fmt.Println("Invalid task ID. Must be a number.")
		return true
	}
	if action != "pause" && action != "resume" && action != "cancel" && action != "reset" {
		fmt.Println("Invalid action. Use 'pause', 'resume', 'cancel', 'reset', or 'speed'.")
		return true
	}
	sendControlMessage(taskID, action)
	return true
}

func sendControlMessage(taskID int, action string) {
	controlMux.Lock()
	if controlChan, ok := controlChans[taskID]; ok {
		controlChan <- ControlMessage{TaskID: taskID, Action: action}
		fmt.Printf("%s command sent for task ID %d\n", action, taskID)
	} else {
		fmt.Printf("Task ID %d not found or already completed/canceled\n", taskID)
	}
	controlMux.Unlock()
}

func monitorDownloads(wg *sync.WaitGroup, results chan DownloadStatus, totalTasks int) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		count := 0
		for result := range results {
			reportDownloadResult(result)
			count++
			if count == totalTasks {
				close(stopDisplay)
			}
		}
	}()
}

func reportDownloadResult(result DownloadStatus) {
	if result.Error != nil {
		fmt.Printf("Download failed for %s after %d retries: %v\n", result.URL, result.RetriesLeft, result.Error)
	} else if result.State == "canceled" {
		fmt.Printf("Download canceled for %s\n", result.URL)
	} else {
		fmt.Printf("Download completed for %s\n", result.URL)
	}
}

// New function for hardcoded speed changes
func applyHardcodedSpeedChanges() {
	// Hardcode speed changes for Queue 1
	go func() {
		time.Sleep(3 * time.Second)
		updateQueueSpeedLimit(1, 600*1024)
		fmt.Println("Queue 1 speed limit updated to 600 KB/s after 3 seconds")

		time.Sleep(3 * time.Second) // Total 6 seconds
		updateQueueSpeedLimit(1, 700*1024)
		fmt.Println("Queue 1 speed limit updated to 700 KB/s after 6 seconds")
	}()
}

func main() {
	downloadQueues := setupDownloadQueues()
	results := make(chan DownloadStatus, 10)
	pool := NewWorkerPool(2)
	var wg sync.WaitGroup

	startProgressDisplay(&wg)
	for i, queue := range downloadQueues {
		pool.Submit(func() {
			processQueue(queue, pool, results)
		})
		if err := validateQueue(queue); err != nil {
			fmt.Printf("Queue %d validation failed: %v\n", i, err)
		}
	}
	handleUserInput(&wg, pool)

	totalTasks := 0
	for _, q := range downloadQueues {
		totalTasks += len(q.Tasks)
	}

	applyHardcodedSpeedChanges()
	monitorDownloads(&wg, results, totalTasks)

	pool.StopWait()
	close(results)
	wg.Wait()
	fmt.Println("All downloads processed.")
}

func validateQueue(queue DownloadQueue) error {
	if queue.SpeedLimit <= 0 {
		return fmt.Errorf("speed limit must be positive")
	}
	if queue.ConcurrentLimit <= 0 {
		return fmt.Errorf("concurrent limit must be positive")
	}
	if queue.MaxRetries < 0 {
		return fmt.Errorf("max retries cannot be negative")
	}
	return nil
}
