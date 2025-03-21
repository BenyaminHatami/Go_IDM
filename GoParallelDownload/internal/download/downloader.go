package download

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"GoParallelDownload/internal/state"
	"GoParallelDownload/pkg/types"
	"GoParallelDownload/pkg/utils"
)

const (
	maxPartRetries    = 3 // Max retries for a single part
	maxNetworkRetries = 5 // Max retries for network-related issues before canceling queue
	retryDelay        = 5 * time.Second
)

func DownloadFile(task types.DownloadTask, controlChan chan types.ControlMessage, results chan<- types.DownloadStatus, queue *types.DownloadQueue) {
	retriesLeft := queue.MaxRetries
	status := types.DownloadStatus{
		ID:          task.ID,
		URL:         task.URL,
		State:       "pending",
		RetriesLeft: retriesLeft,
		PartID:      -1,
	}
	utils.UpdateProgressMap(task.FilePath, status, 0, 0, 0, false, 0, "N/A", "pending")

	for {
		select {
		case msg := <-controlChan:
			if msg.TaskID == task.ID || msg.QueueID == task.QueueID {
				switch msg.Action {
				case "resume":
					if status.State == "pending" || status.State == "paused" {
						status.State = "running"
						utils.UpdateProgressMap(task.FilePath, status, status.Progress, status.BytesDone, status.TotalBytes, status.HasTotalBytes, status.Speed, status.ETA, "running")
						state.ActiveTasks.Store(fmt.Sprintf("%d:%d", queue.ID, task.ID), true)
						fmt.Printf("Task %d resumed in Queue %d\n", task.ID, queue.ID)
					}
				case "pause":
					if status.State == "running" {
						status.State = "paused"
						utils.UpdateProgressMap(task.FilePath, status, status.Progress, status.BytesDone, status.TotalBytes, status.HasTotalBytes, status.Speed, status.ETA, "paused")
						state.ActiveTasks.Delete(fmt.Sprintf("%d:%d", queue.ID, task.ID))
						fmt.Printf("Task %d paused in Queue %d\n", task.ID, queue.ID)
					}
				case "cancel":
					state.ActiveTasks.Delete(fmt.Sprintf("%d:%d", queue.ID, task.ID))
					status.State = "canceled"
					utils.UpdateProgressMap(task.FilePath, status, status.Progress, status.BytesDone, status.TotalBytes, status.HasTotalBytes, 0, "N/A", "canceled")
					results <- status
					utils.CleanupTaskParts(task, queue)
					fmt.Printf("Task %d canceled in Queue %d\n", task.ID, queue.ID)
					return
				case "reset":
					if status.State != "pending" {
						state.ActiveTasks.Delete(fmt.Sprintf("%d:%d", queue.ID, task.ID))
						status.State = "pending"
						utils.UpdateProgressMap(task.FilePath, status, 0, 0, 0, false, 0, "N/A", "pending")
						fmt.Printf("Task %d reset in Queue %d\n", task.ID, queue.ID)
					}
				}
			}
		default:
			if status.State != "running" {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			err := downloadMultipart(task, controlChan, results, queue, &status, retriesLeft)
			if err != nil {
				if err == io.EOF || status.State == "completed" || status.State == "canceled" {
					if status.State == "completed" {
						state.ActiveTasks.Delete(fmt.Sprintf("%d:%d", queue.ID, task.ID))
						results <- status
						fmt.Printf("Task %d completed in Queue %d\n", task.ID, queue.ID)
					} else {
						results <- status
					}
					return
				}
				retriesLeft--
				status.Error = err
				status.State = "failed"
				utils.UpdateProgressMap(task.FilePath, status, status.Progress, status.BytesDone, status.TotalBytes, status.HasTotalBytes, status.Speed, status.ETA, "failed")
				if retriesLeft >= 0 {
					fmt.Printf("Download failed for %s: %v. Retries left: %d. Retrying in %v...\n", task.URL, err, retriesLeft, retryDelay)
					time.Sleep(retryDelay)
					continue
				}
				fmt.Printf("Max retries reached for %s. Canceling download.\n", task.URL)
				controlChan <- types.ControlMessage{TaskID: task.ID, Action: "cancel"}
				status.State = "canceled"
				status.Error = fmt.Errorf("download canceled after %d failed attempts: %v", queue.MaxRetries+1, err)
				results <- status
				utils.CleanupTaskParts(task, queue)
				return
			}
			return
		}
	}
}

func downloadMultipart(task types.DownloadTask, controlChan chan types.ControlMessage, results chan<- types.DownloadStatus, queue *types.DownloadQueue, status *types.DownloadStatus, retriesLeft int) error {
	resp, err := http.Head(task.URL)
	if err != nil {
		return fmt.Errorf("failed to HEAD %s: %v", task.URL, err)
	}
	defer resp.Body.Close()

	totalSize := resp.ContentLength
	if totalSize == -1 || resp.Header.Get("Accept-Ranges") != "bytes" {
		return tryDownload(task, controlChan, results, status, queue)
	}

	finalPath, err := utils.SetupDownloadFile(task, queue)
	if err != nil {
		fmt.Printf("Failed to setup download file for %s: %v. Retrying task.\n", task.URL, err)
		return err // Retry at task level
	}

	partSize := totalSize / int64(types.NumParts)
	parts := make([]types.DownloadPart, types.NumParts)
	for i := 0; i < types.NumParts; i++ {
		start := int64(i) * partSize
		end := start + partSize - 1
		if i == types.NumParts-1 {
			end = totalSize - 1
		}
		parts[i] = types.DownloadPart{
			Start: start,
			End:   end,
			Path:  fmt.Sprintf("%s.part%d", finalPath, i),
		}
	}

	var wg sync.WaitGroup
	partResults := make(chan types.DownloadStatus, types.NumParts)
	partErrors := make(chan error, types.NumParts)
	partControlChans := make([]chan types.ControlMessage, types.NumParts)
	partRetries := make([]int, types.NumParts) // Track retries per part

	for i := range parts {
		partRetries[i] = maxPartRetries
		partControlChans[i] = make(chan types.ControlMessage, 10)
		wg.Add(1)
		go func(partNum int) {
			defer wg.Done()
			for partRetries[partNum] >= 0 {
				err := downloadPart(task, partNum, parts[partNum].Start, parts[partNum].End,
					parts[partNum].Path, partControlChans[partNum], partResults, queue)
				if err == nil {
					return // Part succeeded
				}
				partRetries[partNum]--
				if partRetries[partNum] >= 0 {
					fmt.Printf("Part %d of %s failed: %v. Retries left: %d. Resetting and retrying in %v...\n",
						partNum, task.URL, err, partRetries[partNum], retryDelay)
					if err := os.Remove(parts[partNum].Path); err != nil && !os.IsNotExist(err) {
						fmt.Printf("Failed to clean up failed part %s: %v\n", parts[partNum].Path, err)
					}
					time.Sleep(retryDelay)
					continue
				}
				partErrors <- fmt.Errorf("part %d failed after %d retries: %v", partNum, maxPartRetries, err)
				return
			}
		}(i)
	}

	go func() {
		for msg := range controlChan {
			for _, partChan := range partControlChans {
				partChan <- msg
			}
			if msg.Action == "cancel" {
				break
			}
		}
	}()

	go func() {
		wg.Wait()
		for _, ch := range partControlChans {
			close(ch)
		}
		close(partResults)
		close(partErrors)
	}()

	var totalBytesDone int
	var totalProgress int
	partStatuses := make(map[int]types.DownloadStatus)
	completedParts := 0
	status.TotalBytes = totalSize
	status.HasTotalBytes = true

	for partStatus := range partResults {
		partStatuses[partStatus.PartID] = partStatus
		if partStatus.State == "completed" {
			totalBytesDone += partStatus.BytesDone
			totalProgress += partStatus.Progress / types.NumParts
			completedParts++
		}

		totalSpeed := 0
		for i := 0; i < types.NumParts; i++ {
			if ps, ok := partStatuses[i]; ok {
				totalSpeed += ps.Speed
			}
		}

		eta := "N/A"
		if totalSpeed > 0 && status.HasTotalBytes {
			remainingBytes := totalSize - int64(totalBytesDone)
			if remainingBytes <= 0 {
				eta = "0s"
			} else {
				etaSeconds := float64(remainingBytes) / float64(totalSpeed*1024)
				eta = time.Duration(etaSeconds * float64(time.Second)).Truncate(time.Second).String()
			}
		}

		utils.UpdateProgressMap(task.FilePath, types.DownloadStatus{
			ID:            task.ID,
			URL:           task.URL,
			Progress:      totalProgress,
			BytesDone:     totalBytesDone,
			TotalBytes:    totalSize,
			HasTotalBytes: true,
			State:         "running",
			Speed:         totalSpeed,
			ETA:           eta,
			PartID:        -1,
		}, totalProgress, totalBytesDone, totalSize, true, totalSpeed, eta, "running")
	}

	if len(partErrors) > 0 {
		err := <-partErrors
		fmt.Printf("Multipart download aborted for %s: %v\n", task.URL, err)
		return err // Trigger task-level retry or cancellation
	}

	if completedParts == types.NumParts {
		if err := utils.MergeParts(finalPath, parts); err != nil {
			fmt.Printf("Failed to merge parts for %s: %v. Retrying task.\n", task.URL, err)
			for _, part := range parts {
				os.Remove(part.Path) // Clean up parts
			}
			return err // Retry at task level
		}
		for _, part := range parts {
			if err := os.Remove(part.Path); err != nil && !os.IsNotExist(err) {
				fmt.Printf("Warning: Failed to remove part %s: %v\n", part.Path, err)
			}
		}
		status.State = "completed"
		status.Progress = 100
		status.BytesDone = int(totalSize)
		utils.UpdateProgressMap(task.FilePath, *status, 100, int(totalSize), totalSize, true, 0, "0s", "completed")
		results <- *status
		return nil
	}

	return fmt.Errorf("incomplete download: only %d of %d parts completed", completedParts, types.NumParts)
}

func downloadPart(task types.DownloadTask, partNum int, start, end int64, partPath string, controlChan chan types.ControlMessage, results chan<- types.DownloadStatus, queue *types.DownloadQueue) error {
	req, err := http.NewRequest("GET", task.URL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	networkRetries := maxNetworkRetries
	for networkRetries > 0 {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			networkRetries--
			if networkRetries > 0 {
				fmt.Printf("Network error downloading part %d of %s: %v. Retries left: %d. Retrying in %v...\n",
					partNum, task.URL, err, networkRetries, retryDelay)
				time.Sleep(retryDelay)
				continue
			}
			return fmt.Errorf("network failed after %d retries: %v", maxNetworkRetries, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusPartialContent {
			return fmt.Errorf("server returned %s, expected 206 Partial Content", resp.Status)
		}

		file, err := os.Create(partPath)
		if err != nil {
			return fmt.Errorf("failed to create part file %s: %v", partPath, err)
		}
		defer file.Close()

		partStatus := types.DownloadStatus{
			ID:            task.ID,
			URL:           task.URL,
			TotalBytes:    end - start + 1,
			HasTotalBytes: true,
			State:         "running",
			PartID:        partNum,
			BytesInWindow: []int{},
			Timestamps:    []time.Time{},
		}

		bytesDownloaded := 0
		accumulatedBytes := 0
		lastUpdate := time.Now()
		buffer := make([]byte, types.TokenSize)
		paused := false

		for {
			select {
			case msg := <-controlChan:
				switch msg.Action {
				case "pause":
					if partStatus.State == "running" {
						partStatus.State = "paused"
						paused = true
						utils.UpdateProgressMap(fmt.Sprintf("%s.part%d", task.FilePath, partNum), partStatus,
							partStatus.Progress, bytesDownloaded, partStatus.TotalBytes, true,
							partStatus.Speed, partStatus.ETA, "paused")
					}
				case "resume":
					if partStatus.State == "paused" {
						partStatus.State = "running"
						paused = false
						utils.UpdateProgressMap(fmt.Sprintf("%s.part%d", task.FilePath, partNum), partStatus,
							partStatus.Progress, bytesDownloaded, partStatus.TotalBytes, true,
							partStatus.Speed, partStatus.ETA, "running")
					}
				case "cancel":
					file.Close()
					os.Remove(partPath)
					partStatus.State = "canceled"
					results <- partStatus
					return nil
				case "reset":
					file.Close()
					os.Remove(partPath)
					partStatus.State = "reset"
					return fmt.Errorf("reset requested")
				}
			default:
				if paused || partStatus.State != "running" {
					time.Sleep(100 * time.Millisecond)
					continue
				}

				queue.TokenBucket.WaitForTokens(float64(types.TokenSize))
				n, err := resp.Body.Read(buffer)
				if n > 0 {
					if _, err := file.Write(buffer[:n]); err != nil {
						return fmt.Errorf("failed to write to part %s: %v", partPath, err)
					}
					bytesDownloaded += n
					accumulatedBytes += n
					utils.UpdateRollingWindow(&partStatus.BytesInWindow, &partStatus.Timestamps, n, time.Now())
					if accumulatedBytes >= utils.UpdateIntervalBytes || time.Since(lastUpdate) >= utils.UpdateIntervalTime {
						progress := utils.CalculateProgress(bytesDownloaded, partStatus.TotalBytes, true)
						speed, eta := utils.CalculateSpeedAndETA(partStatus.BytesInWindow, partStatus.Timestamps,
							bytesDownloaded, partStatus.TotalBytes, true)
						utils.UpdateProgressMap(fmt.Sprintf("%s.part%d", task.FilePath, partNum), partStatus,
							progress, bytesDownloaded, partStatus.TotalBytes, true, speed, eta, "running")
						partStatus.Progress = progress
						partStatus.Speed = speed
						partStatus.ETA = eta
						partStatus.BytesDone = bytesDownloaded
						results <- partStatus
						accumulatedBytes = 0
						lastUpdate = time.Now()
					}
				}
				if err != nil {
					if err == io.EOF {
						partStatus.State = "completed"
						partStatus.Progress = 100
						partStatus.BytesDone = bytesDownloaded
						utils.UpdateProgressMap(fmt.Sprintf("%s.part%d", task.FilePath, partNum), partStatus,
							100, bytesDownloaded, partStatus.TotalBytes, true, 0, "0s", "completed")
						results <- partStatus
						return nil
					}
					return fmt.Errorf("read error: %v", err)
				}
			}
		}
	}
	return fmt.Errorf("unexpected exit from network retry loop")
}

func tryDownload(task types.DownloadTask, controlChan chan types.ControlMessage, results chan<- types.DownloadStatus, status *types.DownloadStatus, queue *types.DownloadQueue) error {
	fullPath, err := utils.SetupDownloadFile(task, queue)
	if err != nil {
		fmt.Printf("Failed to setup file %s: %v. Retrying task.\n", task.URL, err)
		return err
	}

	networkRetries := maxNetworkRetries
	for networkRetries > 0 {
		file, resp, err := initiateDownload(fullPath, task.URL)
		if err != nil {
			networkRetries--
			if networkRetries > 0 {
				fmt.Printf("Failed to initiate download %s: %v. Retries left: %d. Retrying in %v...\n",
					task.URL, err, networkRetries, retryDelay)
				time.Sleep(retryDelay)
				continue
			}
			fmt.Printf("Network failed for %s after %d retries. Canceling task.\n", task.URL, maxNetworkRetries)
			return fmt.Errorf("network failed after %d retries: %v", maxNetworkRetries, err)
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

		err = processDownload(task, file, resp, controlChan, results, status, &fileClosed, fullPath, queue)
		if err == nil || status.State == "canceled" {
			return err // Success or canceled
		}
		networkRetries--
		if networkRetries > 0 {
			fmt.Printf("Single-file download %s failed: %v. Retries left: %d. Retrying in %v...\n",
				task.URL, err, networkRetries, retryDelay)
			if err := file.Close(); err != nil {
				fmt.Printf("Error closing file %s: %v\n", fullPath, err)
			}
			fileClosed = true
			if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
				fmt.Printf("Failed to clean up %s: %v\n", fullPath, err)
			}
			time.Sleep(retryDelay)
			continue
		}
		fmt.Printf("Max retries reached for %s. Canceling task.\n", task.URL)
		return fmt.Errorf("failed after %d retries: %v", maxNetworkRetries, err)
	}
	return fmt.Errorf("unexpected exit from retry loop")
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

func processDownload(task types.DownloadTask, file *os.File, resp *http.Response, controlChan chan types.ControlMessage, results chan<- types.DownloadStatus, status *types.DownloadStatus, fileClosed *bool, fullPath string, queue *types.DownloadQueue) error {
	status.TotalBytes = resp.ContentLength
	status.HasTotalBytes = status.TotalBytes != -1
	status.BytesInWindow = []int{}
	status.Timestamps = []time.Time{}

	bytesDownloaded := 0
	accumulatedBytes := 0
	lastUpdate := time.Now()
	buffer := make([]byte, types.TokenSize)

	for {
		select {
		case msg := <-controlChan:
			if msg.TaskID == task.ID || msg.QueueID == task.QueueID {
				if msg.Action == "reset" {
					*fileClosed = true
					if err := file.Close(); err != nil {
						fmt.Printf("Error closing file %s during reset: %v\n", fullPath, err)
					}
					if err := utils.SafeRemoveFile(fullPath); err != nil {
						fmt.Printf("Error removing file %s during reset: %v\n", fullPath, err)
					}
					status.State = "reset"
					status.Progress = 0
					status.BytesDone = 0
					status.BytesInWindow = []int{}
					status.Timestamps = []time.Time{}
					status.Speed = 0
					status.ETA = "N/A"
					utils.UpdateProgressMap(task.FilePath, *status, 0, 0, status.TotalBytes, status.HasTotalBytes, 0, "N/A", "reset")
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
			if err := downloadChunk(task, file, resp.Body, buffer, status, &bytesDownloaded, &accumulatedBytes, &lastUpdate, queue); err != nil {
				if err == io.EOF {
					utils.UpdateProgressMap(task.FilePath, *status, 100, bytesDownloaded, status.TotalBytes, status.HasTotalBytes, 0, "0s", "completed")
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

func downloadChunk(task types.DownloadTask, file *os.File, body io.Reader, buffer []byte, status *types.DownloadStatus, bytesDownloaded, accumulatedBytes *int, lastUpdate *time.Time, queue *types.DownloadQueue) error {
	queue.TokenBucket.WaitForTokens(float64(types.TokenSize))
	n, err := utils.ReadAndWriteChunk(body, file, buffer)
	if err != nil {
		return err
	}
	*bytesDownloaded += n
	*accumulatedBytes += n
	utils.UpdateRollingWindow(&status.BytesInWindow, &status.Timestamps, n, time.Now())
	if *accumulatedBytes >= utils.UpdateIntervalBytes || time.Since(*lastUpdate) >= utils.UpdateIntervalTime {
		utils.UpdateProgress(status, task.FilePath, *bytesDownloaded, status.TotalBytes, status.HasTotalBytes)
		*accumulatedBytes = 0
		*lastUpdate = time.Now()
	}
	return nil
}

func handleControlMessage(status *types.DownloadStatus, task types.DownloadTask, file *os.File, fullPath string, bytesDownloaded int, totalBytes int64, hasTotalBytes bool, controlChan chan types.ControlMessage, msg types.ControlMessage) error {
	progress := utils.CalculateProgress(bytesDownloaded, totalBytes, hasTotalBytes)
	speed, eta := utils.CalculateSpeedAndETA(status.BytesInWindow, status.Timestamps, bytesDownloaded, totalBytes, hasTotalBytes)

	switch msg.Action {
	case "pause":
		state.ProgressMux.Lock()
		state.ProgressMap[task.FilePath] = utils.NewDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, speed, eta, "paused")
		state.ProgressMux.Unlock()
		for {
			resumeMsg := <-controlChan
			if resumeMsg.TaskID == task.ID || resumeMsg.QueueID == task.QueueID {
				if resumeMsg.Action == "resume" {
					state.ProgressMux.Lock()
					state.ProgressMap[task.FilePath] = utils.NewDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, speed, eta, "running")
					state.ProgressMux.Unlock()
					*status = utils.NewDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, speed, eta, "running")
					return nil
				} else if resumeMsg.Action == "cancel" {
					file.Close()
					if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
						return fmt.Errorf("failed to remove file %s during cancel: %v", fullPath, err)
					}
					state.ProgressMux.Lock()
					state.ProgressMap[task.FilePath] = utils.NewDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, 0, "N/A", "canceled")
					state.ProgressMux.Unlock()
					*status = utils.NewDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, 0, "N/A", "canceled")
					return nil
				}
			}
		}
	case "cancel":
		file.Close()
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove file %s during cancel: %v", fullPath, err)
		}
		state.ProgressMux.Lock()
		state.ProgressMap[task.FilePath] = utils.NewDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, 0, "N/A", "canceled")
		state.ProgressMux.Unlock()
		*status = utils.NewDownloadStatus(task, status, progress, bytesDownloaded, totalBytes, hasTotalBytes, 0, "N/A", "canceled")
		return nil
	}
	return nil
}
