package queue_tab

import (
	"fmt"
	"myproject/internal/base"
	"strings"
	"time"
)

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

// progressBar generates a progress bar string or bytes downloaded if size is unknown
func ProgressBar(status base.DownloadStatus) string {
	if !status.HasTotalBytes {
		return fmt.Sprintf("%s downloaded", FormatSize(status.BytesDone))
	}
	const width = 20
	completed := status.Progress * width / 100
	bar := strings.Repeat("=", completed) + strings.Repeat("-", width-completed)
	return fmt.Sprintf("[%s] %d%%", bar, status.Progress)
}

// displayProgress updates the terminal with the progress of all downloads

func DisplayProgress() {
	for {
		fmt.Print("\033[H\033[2J") // Clear the terminal
		base.ProgressMux.Lock()
		for filePath, status := range base.ProgressMap {
			fmt.Printf("Downloading %s (ID: %d): %s (%d KB/s) ETA: %s State: %s\n", filePath, status.ID, ProgressBar(status), status.Speed, status.ETA, status.State)
		}
		base.ProgressMux.Unlock()
		fmt.Println("\nEnter command (e.g., 'pause 1', 'resume 1', 'cancel 1', or 'exit'):")
		time.Sleep(500 * time.Millisecond)
	}
}
