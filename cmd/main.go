package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"time"

	ui "myproject/internal/ui/download_tab"

	"github.com/rivo/tview"
	"github.com/schollz/progressbar/v3"
)

func downloadFile(url string) error {

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("error making request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	totalSize := resp.ContentLength
	if totalSize <= 0 {
		return fmt.Errorf("unknown file size")
	}

	filename := path.Base(url)
	if filename == "" || filename == "." || filename == "/" {
		filename = "downloaded_file"
	}

	out, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("error creating file: %v", err)
	}
	defer out.Close()

	bar := progressbar.NewOptions64(
		totalSize,
		progressbar.OptionSetWidth(30),
		progressbar.OptionShowBytes(true),
		progressbar.OptionShowCount(),
		progressbar.OptionSetDescription(fmt.Sprintf("Downloading %s", filename)),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionShowElapsedTimeOnFinish(),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "=",
			SaucerHead:    ">",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)

	start := time.Now()
	multiWriter := io.MultiWriter(out, bar)

	reader := io.TeeReader(resp.Body, multiWriter)

	_, err = io.Copy(out, reader)
	if err != nil {
		return fmt.Errorf("error downloading file: %v", err)
	}

	bar.Finish()
	fmt.Println()

	elapsed := time.Since(start)
	speed := float64(totalSize) / elapsed.Seconds() / 1024 // KB/s
	fmt.Printf("Download completed in %v\n", elapsed.Round(time.Second))
	fmt.Printf("Average speed: %.2f KB/s\n", speed)

	return nil
}

func main() {
	app := tview.NewApplication()
	ui.CreateForm(app)
}
