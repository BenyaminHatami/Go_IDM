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

// DownloadTask represents a single download task
type DownloadTask struct {
	ID          int
	URL         string
	FilePath    string
	FileType    string
	TokenBucket *TokenBucket
	QueueID     int
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
	State         string // "pending", "running", "paused", "completed", "canceled", "failed"
	Speed         int
	ETA           string
	BytesInWindow []int
	Timestamps    []time.Time
	Error         error
	RetriesLeft   int
}

// ControlMessage represents a control command
type ControlMessage struct {
	TaskID  int
	QueueID int // Optional: -1 means task-specific, otherwise queue-specific
	Action  string
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
	stopDisplay    = make(chan struct{})
	activeTasks    = sync.Map{}                   // Map[queueID:taskID]bool
	queues         = make(map[int]*DownloadQueue) // Store queue pointers
	bandwidthChans = make(map[int]chan struct{})
	bandwidthMux   = sync.Mutex{}
)

// TokenBucket represents a token bucket system
type TokenBucket struct {
	mu             sync.Mutex
	tokens         float64
	maxTokens      float64
	refillRate     float64
	lastRefillTime time.Time
	quit           chan struct{}
}

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

func (tb *TokenBucket) Stop() {
	close(tb.quit)
}

func (tb *TokenBucket) Request(tokens float64) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if tokens <= tb.tokens {
		tb.tokens -= tokens
		return true
	}
	return false
}

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

// WorkerPool implementation (unchanged)
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

func (p *WorkerPool) Submit(task func()) {
	if task != nil {
		p.taskQueue <- task
	}
}

func (p *WorkerPool) StopWait() {
	p.stop(true)
}

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

func worker(task func(), workerQueue chan func(), wg *sync.WaitGroup) {
	for task != nil {
		task()
		task = <-workerQueue
	}
	wg.Done()
}

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

func downloadFile(task DownloadTask, controlChan chan ControlMessage, results chan<- DownloadStatus, queue *DownloadQueue) {
	retriesLeft := queue.MaxRetries
	// Start in pending state
	status := DownloadStatus{
		ID:          task.ID,
		URL:         task.URL,
		State:       "pending",
		RetriesLeft: retriesLeft,
	}
	updateProgressMap(task.FilePath, status, 0, 0, 0, false, 0, "N/A", "pending")

	for {
		// Wait for "resume" to start or handle queue-level commands
		msg := <-controlChan
		if msg.TaskID == task.ID || msg.QueueID == task.QueueID {
			switch msg.Action {
			case "resume":
				if status.State == "pending" || status.State == "paused" {
					status.State = "running"
					updateProgressMap(task.FilePath, status, status.Progress, status.BytesDone, status.TotalBytes, status.HasTotalBytes, status.Speed, status.ETA, "running")
					break
				}
				continue
			case "pause":
				if status.State == "running" {
					status.State = "paused"
					updateProgressMap(task.FilePath, status, status.Progress, status.BytesDone, status.TotalBytes, status.HasTotalBytes, status.Speed, status.ETA, "paused")
				}
				continue
			case "cancel":
				status.State = "canceled"
				updateProgressMap(task.FilePath, status, status.Progress, status.BytesDone, status.TotalBytes, status.HasTotalBytes, 0, "N/A", "canceled")
				results <- status
				activeTasks.Delete(fmt.Sprintf("%d:%d", queue.ID, task.ID))
				triggerBandwidthRedistribution(queue.ID)
				return
			case "reset":
				if status.State != "pending" {
					status.State = "pending"
					updateProgressMap(task.FilePath, status, 0, 0, 0, false, 0, "N/A", "pending")
				}
				continue
			}
		}

		if status.State != "running" {
			continue
		}

		activeTasks.Store(fmt.Sprintf("%d:%d", queue.ID, task.ID), true)
		triggerBandwidthRedistribution(queue.ID)

		err := tryDownload(task, controlChan, results, &status)
		if err != nil && status.State == "reset" {
			fmt.Printf("Resetting download for task %d\n", task.ID)
			continue
		}
		if err != nil {
			if err == io.EOF || status.State == "completed" || status.State == "canceled" {
				activeTasks.Delete(fmt.Sprintf("%d:%d", queue.ID, task.ID))
				triggerBandwidthRedistribution(queue.ID)
				return
			}
			retriesLeft--
			status.Error = err
			status.State = "failed"
			updateProgressMap(task.FilePath, status, status.Progress, status.BytesDone, status.TotalBytes, status.HasTotalBytes, status.Speed, status.ETA, "failed")
			if retriesLeft >= 0 {
				fmt.Printf("Download failed for %s: %v. Retries left: %d. Retrying in %v...\n", task.URL, err, retriesLeft, retryDelay)
				time.Sleep(retryDelay)
				continue
			}
			fmt.Printf("Max retries reached for %s. Canceling download.\n", task.URL)
			controlChan <- ControlMessage{TaskID: task.ID, Action: "cancel"}
			status.State = "canceled"
			status.Error = fmt.Errorf("download canceled after %d failed attempts: %v", queue.MaxRetries+1, err)
			results <- status
			activeTasks.Delete(fmt.Sprintf("%d:%d", queue.ID, task.ID))
			triggerBandwidthRedistribution(queue.ID)
			return
		}
		activeTasks.Delete(fmt.Sprintf("%d:%d", queue.ID, task.ID))
		triggerBandwidthRedistribution(queue.ID)
		return
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

func setupDownloadFile(task DownloadTask, queue *DownloadQueue) (string, error) {
	baseDir := queue.DownloadLocation
	if baseDir == "" {
		baseDir = "."
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
			if msg.TaskID == task.ID || msg.QueueID == task.QueueID {
				if msg.Action == "reset" {
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
	for i := 0; i < 5; i++ {
		err := os.Remove(filePath)
		if err == nil {
			return nil
		}
		if strings.Contains(err.Error(), "being used by another process") {
			fmt.Printf("File %s in use, retrying removal (%d/%d)...\n", filePath, i+1, 5)
			time.Sleep(time.Millisecond * 500)
			continue
		}
		return fmt.Errorf("failed to remove file %s: %v", filePath, err)
	}
	return fmt.Errorf("failed to remove file %s after 5 attempts: file in use", filePath)
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
			if resumeMsg.TaskID == task.ID || resumeMsg.QueueID == task.QueueID {
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
			progressMux.Lock()
			if len(progressMap) == 0 {
				fmt.Println("No downloads.")
			} else {
				fmt.Println("Active Downloads:")
				hasActive := false
				for filePath, status := range progressMap {
					if status.State != "pending" {
						hasActive = true
						fmt.Printf("  %s (ID: %d): %s (%d KB/s) ETA: %s State: %s\n",
							filePath, status.ID, progressBar(status), status.Speed, status.ETA, status.State)
					}
				}
				if !hasActive {
					fmt.Println("  None")
				}

				fmt.Println("Pending Downloads:")
				hasPending := false
				for filePath, status := range progressMap {
					if status.State == "pending" {
						hasPending = true
						fmt.Printf("  %s (ID: %d): State: %s\n", filePath, status.ID, status.State)
					}
				}
				if !hasPending {
					fmt.Println("  None")
				}
			}
			progressMux.Unlock()
			fmt.Println("\nEnter command (e.g., 'pausequeue 1', 'resumequeue 1', 'cancelqueue 1', 'speed 1 600', 'settime 1 10:00 12:00', 'setretries 1 5', or 'exit'):")
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

func startTimeCheck(queue *DownloadQueue) {
	if queue.StartTime != nil || queue.StopTime != nil {
		go func() {
			for {
				checkQueueTimeWindow(queue)
				time.Sleep(10 * time.Second)
			}
		}()
	}
}

func processQueue(queue *DownloadQueue, pool *WorkerPool, results chan<- DownloadStatus) {
	queuePool, err := setupQueuePool(queue)
	if err != nil {
		fmt.Printf("Failed to setup queue pool: %v\n", err)
		return
	}
	startTimeCheck(queue)
	submitQueueTasks(queue, queuePool, results)
	queuePool.StopWait()
}

func setupQueuePool(queue *DownloadQueue) (*WorkerPool, error) {
	if len(queue.Tasks) == 0 {
		return nil, fmt.Errorf("queue has no tasks")
	}
	initialBandwidth := queue.SpeedLimit / float64(queue.ConcurrentLimit)
	for i := range queue.Tasks {
		queue.Tasks[i].QueueID = queue.ID
		queue.Tasks[i].TokenBucket = NewTokenBucket(initialBandwidth, initialBandwidth)
	}
	queues[queue.ID] = queue

	bandwidthChans[queue.ID] = make(chan struct{}, 1)
	go manageBandwidthRedistribution(queue.ID)

	time.Sleep(100 * time.Millisecond)
	triggerBandwidthRedistribution(queue.ID)

	return NewWorkerPool(queue.ConcurrentLimit), nil
}

func triggerBandwidthRedistribution(queueID int) {
	bandwidthMux.Lock()
	if ch, exists := bandwidthChans[queueID]; exists {
		select {
		case ch <- struct{}{}:
		default:
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
		fmt.Printf("Updated speed limit for Queue %d to %f KB/s\n", queueID, newSpeedLimit/1024)
		triggerBandwidthRedistribution(queueID)
	} else {
		fmt.Printf("Queue %d not found\n", queueID)
	}
}

func redistributeBandwidth(queue *DownloadQueue) {
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
		return
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
						task.TokenBucket.tokens = math.Min(task.TokenBucket.tokens, newBandwidthPerTask)
						task.TokenBucket.refillRate = newBandwidthPerTask
						task.TokenBucket.mu.Unlock()
						fmt.Printf("Task %d updated to %f KB/s (active tasks: %d)\n", taskID, newBandwidthPerTask/1024, activeCount)
					}
				}
			}
		}
		return true
	})
}

func checkQueueTimeWindow(queue *DownloadQueue) {
	now := time.Now()
	withinTimeWindow := true
	if queue.StartTime != nil {
		startToday := time.Date(now.Year(), now.Month(), now.Day(), queue.StartTime.Hour(), queue.StartTime.Minute(), 0, 0, time.Local)
		if now.Before(startToday) {
			withinTimeWindow = false
		}
	}
	if queue.StopTime != nil {
		stopToday := time.Date(now.Year(), now.Month(), now.Day(), queue.StopTime.Hour(), queue.StopTime.Minute(), 0, 0, time.Local)
		if now.After(stopToday) {
			withinTimeWindow = false
		}
	}
	controlQueueTasks(queue, withinTimeWindow)
}

func controlQueueTasks(queue *DownloadQueue, withinTimeWindow bool) {
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

func submitQueueTasks(queue *DownloadQueue, queuePool *WorkerPool, results chan<- DownloadStatus) {
	for _, task := range queue.Tasks {
		controlChan := make(chan ControlMessage, 10) // Larger buffer for queue commands
		controlMux.Lock()
		controlChans[task.ID] = controlChan
		controlMux.Unlock()
		queuePool.Submit(func() {
			downloadFile(task, controlChan, results, queue)
			cleanupTask(task)
		})
	}
}

func cleanupTask(task DownloadTask) {
	controlMux.Lock()
	defer controlMux.Unlock()

	if controlChan, exists := controlChans[task.ID]; exists {
		delete(controlChans, task.ID)
		close(controlChan)
	}

	progressMux.Lock()
	if status, ok := progressMap[task.FilePath]; ok && (status.State == "completed" || status.State == "canceled") {
		delete(progressMap, task.FilePath)
	}
	progressMux.Unlock()

	if task.TokenBucket != nil {
		task.TokenBucket.Stop()
	}

	activeTasks.Delete(fmt.Sprintf("%d:%d", task.QueueID, task.ID))
	if queue, exists := queues[task.QueueID]; exists {
		redistributeBandwidth(queue)
	}
}

func updateProgress(status *DownloadStatus, filePath string, bytesDownloaded int, totalBytes int64, hasTotalBytes bool) {
	progress := calculateProgress(bytesDownloaded, totalBytes, hasTotalBytes)
	speed, eta := calculateSpeedAndETA(status.BytesInWindow, status.Timestamps, bytesDownloaded, totalBytes, hasTotalBytes)
	updateProgressMap(filePath, *status, progress, bytesDownloaded, totalBytes, hasTotalBytes, speed, eta, status.State)
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

func updateQueueTimeInterval(queueID int, startTime, stopTime *time.Time) {
	if queue, exists := queues[queueID]; exists {
		queue.StartTime = startTime
		queue.StopTime = stopTime
		fmt.Printf("Updated time interval for Queue %d to Start: %s, Stop: %s\n",
			queueID, startTime.Format("15:04"), stopTime.Format("15:04"))
		checkQueueTimeWindow(queue)
	} else {
		fmt.Printf("Queue %d not found\n", queueID)
	}
}

func updateQueueRetries(queueID, retries int) {
	if retries < 0 {
		fmt.Println("Retries cannot be negative")
		return
	}
	if queue, exists := queues[queueID]; exists {
		queue.MaxRetries = retries
		fmt.Printf("Updated max retries for Queue %d to %d\n", queueID, retries)
	} else {
		fmt.Printf("Queue %d not found\n", queueID)
	}
}

func setupDownloadQueues() []*DownloadQueue {
	now := time.Now()
	startTime1 := now
	stopTime1 := now.Add(2 * time.Hour)
	queuesList := []*DownloadQueue{
		{
			Tasks: []DownloadTask{
				{ID: 1, URL: "https://dl.sevilmusics.com/cdn/music/srvrf/Sohrab%20Pakzad%20-%20Dokhtar%20Irooni%20[SevilMusic]%20[Remix].mp3"},
				{ID: 2, URL: "https://dls.musics-fa.com/tagdl/downloads/Homayoun%20Shajarian%20-%20Chera%20Rafti%20(128).mp3"},
				{ID: 3, URL: "https://soft1.downloadha.com/NarmAfzaar/June2020/Adobe.Photoshop.2020.v21.2.0.225.x64.Portable_www.Downloadha.com_.rar"},
			},
			SpeedLimit:       float64(totalBandwidthLimit),
			ConcurrentLimit:  2,
			StartTime:        &startTime1,
			StopTime:         &stopTime1,
			MaxRetries:       3,
			ID:               1,
			DownloadLocation: "C:/CustomDownloads",
		},
	}
	for i := range queuesList {
		setFileNames(queuesList[i].Tasks)
		setFileType(queuesList[i].Tasks)
	}
	return queuesList
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
		fmt.Println("Invalid command. Use 'pausequeue QUEUE_ID', 'resumequeue QUEUE_ID', 'cancelqueue QUEUE_ID', 'speed QUEUE_ID KB_PER_SEC', 'settime QUEUE_ID START_HH:MM STOP_HH:MM', 'setretries QUEUE_ID NUM', or 'task-specific commands'")
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
		sendQueueControlMessage(queueID, action)
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
		updateQueueSpeedLimit(queueID, float64(speedKB*1024))
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
		updateQueueTimeInterval(queueID, startTime, stopTime)
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
		updateQueueRetries(queueID, retries)
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
		sendControlMessage(taskID, -1, action)
	}
	return true
}

func sendControlMessage(taskID, queueID int, action string) {
	controlMux.Lock()
	defer controlMux.Unlock()
	if controlChan, ok := controlChans[taskID]; ok {
		controlChan <- ControlMessage{TaskID: taskID, QueueID: queueID, Action: action}
		fmt.Printf("%s command sent for task ID %d\n", action, taskID)
	} else {
		fmt.Printf("Task ID %d not found or already completed/canceled\n", taskID)
	}
}

func sendQueueControlMessage(queueID int, action string) {
	controlMux.Lock()
	defer controlMux.Unlock()
	if queue, exists := queues[queueID]; exists {
		for _, task := range queue.Tasks {
			if controlChan, ok := controlChans[task.ID]; ok {
				controlChan <- ControlMessage{TaskID: task.ID, QueueID: queueID, Action: action}
			}
		}
		fmt.Printf("%s command sent for all tasks in Queue %d\n", action, queueID)
	} else {
		fmt.Printf("Queue %d not found\n", queueID)
	}
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

func applyHardcodedSpeedChanges() {
	go func() {
		time.Sleep(5 * time.Second)
		updateQueueSpeedLimit(1, 600*1024)
		fmt.Println("Queue 1 speed limit updated to 600 KB/s after 5 seconds")

		time.Sleep(3 * time.Second)
		startTime, stopTime, _ := parseTimeInterval("10:00", "12:00")
		updateQueueTimeInterval(1, startTime, stopTime)
		fmt.Println("Queue 1 time interval updated to 10:00-12:00 after 6 seconds")

		time.Sleep(3 * time.Second)
		updateQueueRetries(1, 5)
		fmt.Println("Queue 1 max retries updated to 5 after 9 seconds")

		time.Sleep(3 * time.Second)
		updateQueueSpeedLimit(1, 700*1024)
		fmt.Println("Queue 1 speed limit updated to 700 KB/s after 12 seconds")

		time.Sleep(3 * time.Second)
		sendQueueControlMessage(1, "resume")
		fmt.Println("Queue 1 all tasks resumed after 15 seconds")

		time.Sleep(3 * time.Second)
		sendQueueControlMessage(1, "pause")
		fmt.Println("Queue 1 all tasks paused after 18 seconds")

		time.Sleep(3 * time.Second)
		sendQueueControlMessage(1, "resume")
		fmt.Println("Queue 1 all tasks resumed again after 21 seconds")
	}()
}

func main() {
	downloadQueues := setupDownloadQueues()
	results := make(chan DownloadStatus, 10)
	pool := NewWorkerPool(2)
	var wg sync.WaitGroup

	startProgressDisplay(&wg)
	for _, queue := range downloadQueues {
		pool.Submit(func() {
			processQueue(queue, pool, results)
		})
		if err := validateQueue(queue); err != nil {
			fmt.Printf("Queue %d validation failed: %v\n", queue.ID, err)
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

func validateQueue(queue *DownloadQueue) error {
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

//TODO FIX
