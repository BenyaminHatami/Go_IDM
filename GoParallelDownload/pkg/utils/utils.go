package utils

import (
	state2 "GoParallelDownload/internal/state"
	"GoParallelDownload/pkg/types"
	"fmt"
	"io"

	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	WindowDuration      = 1 * time.Second
	UpdateIntervalBytes = 10 * 1024
	UpdateIntervalTime  = 100 * time.Millisecond
	RetryDelay          = 5 * time.Second
)

func SetupDownloadFile(task types.DownloadTask, queue *types.DownloadQueue) (string, error) {
	baseDir := queue.DownloadLocation
	if baseDir == "" {
		baseDir = "."
	}
	fullDir := filepath.Join(baseDir, types.SubDirDownloads, task.FileType)
	if err := os.MkdirAll(fullDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create directory %s: %v", fullDir, err)
	}
	return filepath.Join(fullDir, task.FilePath), nil
}

func MergeParts(finalPath string, parts []types.DownloadPart) error {
	outFile, err := os.Create(finalPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	for _, part := range parts {
		inFile, err := os.Open(part.Path)
		if err != nil {
			return err
		}
		_, err = io.Copy(outFile, inFile)
		inFile.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func SafeRemoveFile(filePath string) error {
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

func CalculateProgress(bytesDownloaded int, totalBytes int64, hasTotalBytes bool) int {
	if !hasTotalBytes || totalBytes <= 0 {
		return 0
	}
	if int64(bytesDownloaded) >= totalBytes {
		return 100
	}
	return int(float64(bytesDownloaded) / float64(totalBytes) * 100)
}

func CalculateSpeedAndETA(bytesInWindow []int, timestamps []time.Time, bytesDownloaded int, totalBytes int64, hasTotalBytes bool) (int, string) {
	if len(timestamps) == 0 {
		return 0, "N/A"
	}

	var totalBytesInWindow int
	for _, b := range bytesInWindow {
		totalBytesInWindow += b
	}
	now := time.Now()
	var windowSeconds float64
	if len(timestamps) > 1 {
		windowSeconds = now.Sub(timestamps[0]).Seconds()
	} else {
		windowSeconds = now.Sub(timestamps[0]).Seconds()
	}
	if windowSeconds <= 0 {
		windowSeconds = 0.001
	}

	speed := int(float64(totalBytesInWindow) / windowSeconds / 1024)
	var eta string
	if hasTotalBytes && speed > 0 && totalBytes > 0 {
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

func UpdateRollingWindow(bytesInWindow *[]int, timestamps *[]time.Time, n int, now time.Time) {
	*bytesInWindow = append(*bytesInWindow, n)
	*timestamps = append(*timestamps, now)
	cutoff := now.Add(-WindowDuration)
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

func NewDownloadStatus(task types.DownloadTask, status *types.DownloadStatus, progress int, bytesDownloaded int, totalBytes int64, hasTotalBytes bool, speed int, eta string, state string) types.DownloadStatus {
	return types.DownloadStatus{
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
		PartID:        status.PartID,
	}
}

func UpdateProgressMap(filePath string, status types.DownloadStatus, progress int, bytesDownloaded int, totalBytes int64, hasTotalBytes bool, speed int, eta string, state string) {
	state2.ProgressMux.Lock()
	state2.ProgressMap[filePath] = types.DownloadStatus{
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
		PartID:        status.PartID,
	}
	state2.ProgressMux.Unlock()
}

func FormatSize(bytes int) string {
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

func ProgressBar(status types.DownloadStatus) string {
	if !status.HasTotalBytes {
		return fmt.Sprintf("%s downloaded", FormatSize(status.BytesDone))
	}
	const width = 20
	completed := status.Progress * width / 100
	bar := strings.Repeat("=", completed) + strings.Repeat("-", width-completed)
	return fmt.Sprintf("[%s] %d%%", bar, status.Progress)
}

func CleanupTaskParts(task types.DownloadTask, queue *types.DownloadQueue) {
	finalPath := filepath.Join(queue.DownloadLocation, types.SubDirDownloads, task.FileType, task.FilePath)
	for i := 0; i < types.NumParts; i++ {
		partPath := fmt.Sprintf("%s.part%d", finalPath, i)
		os.Remove(partPath)
	}
}

func UpdateProgress(status *types.DownloadStatus, filePath string, bytesDownloaded int, totalBytes int64, hasTotalBytes bool) {
	progress := CalculateProgress(bytesDownloaded, totalBytes, hasTotalBytes)
	speed, eta := CalculateSpeedAndETA(status.BytesInWindow, status.Timestamps, bytesDownloaded, totalBytes, hasTotalBytes)
	UpdateProgressMap(filePath, *status, progress, bytesDownloaded, totalBytes, hasTotalBytes, speed, eta, status.State)
}

func ReadAndWriteChunk(body io.Reader, file *os.File, buffer []byte) (int, error) {
	n, err := body.Read(buffer)
	if n > 0 {
		if _, err := file.Write(buffer[:n]); err != nil {
			return n, err
		}
	}
	return n, err
}
