package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

var (
	downloadCompleted = 0
	url               = "https://dl.sevilmusics.com/cdn/music/srvrf/Sohrab%20Pakzad%20-%20Dokhtar%20Irooni%20[SevilMusic]%20[Remix].mp3"
)

func isAcceptRangeSupported() (bool, int) {
	req, _ := http.NewRequest("HEAD", url, nil)
	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println(err)
		return false, 0
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		fmt.Println("Cannot continue download, status code " + strconv.Itoa(resp.StatusCode))
		return false, 0
	}

	acceptRanges := strings.ToLower(resp.Header.Get("Accept-Ranges"))
	if acceptRanges == "" || acceptRanges == "none" {
		return false, int(resp.ContentLength)
	}

	return true, int(resp.ContentLength)
}

func downloadPart(start int, end int, done chan bool) {
	download(strconv.Itoa(int(start)) + "-" + strconv.Itoa(int(end)))
	done <- true
}

func download(opts ...string) {
	req, _ := http.NewRequest("GET", url, nil)
	fileName := "Downloaded_Single_File"
	if len(opts) > 0 {
		req.Header.Add("Range", "bytes="+opts[0])
		fileName = opts[0] + ".part"
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println(err)
		downloadCompleted -= 1
	} else {
		downloadCompleted += 1
	}

	defer resp.Body.Close()

	if resp.Header.Get("Content-Range") != "" {
		fmt.Println(resp.Header.Get("Content-Range"))
	}

	if resp.ContentLength > 0 {
		fmt.Println("Content Length: " + resp.Header.Get("Content-Length"))
	}

	file, err := os.Create(fileName)
	if err != nil {
		fmt.Println(err)
	}
	defer file.Close()

	io.Copy(file, resp.Body)
}

func mergeFiles(fileSize int, concurentConn int) error {
	output, err := os.Create("final_file.tar.gz")
	if err != nil {
		return err
	}
	defer output.Close()

	partSize := fileSize / concurentConn
	for i := 0; i < concurentConn; i++ {
		start := i * partSize
		end := (i+1)*partSize - 1
		if i == concurentConn-1 {
			end = fileSize - 1
		}
		partFileName := strconv.Itoa(start) + "-" + strconv.Itoa(end) + ".part"
		partFile, err := os.Open(partFileName)
		if err != nil {
			return err
		}
		defer partFile.Close()
		_, err = io.Copy(output, partFile)
		if err != nil {
			return err
		}
		os.Remove(partFileName) // Clean up part file
	}
	return nil
}

func Run() (bool, error) {
	defer debug.FreeOSMemory()
	downloadCompleted = 0

	concurentConn := 4

	acceptRangeSupported, fileSize := isAcceptRangeSupported()

	begin := time.Now()

	if fileSize > 0 {
		if acceptRangeSupported {
			fmt.Println("Accept-Ranges supported")

			partSize := fileSize / concurentConn
			done := make(chan bool, concurentConn)

			for i := 0; i < concurentConn; i++ {
				start := i * partSize
				end := (i+1)*partSize - 1
				if i == concurentConn-1 {
					end = fileSize - 1
				}
				go downloadPart(start, end, done)
			}
			for i := 0; i < concurentConn; i++ {
				<-done
			}
			err := mergeFiles(fileSize, concurentConn)
			if err != nil {
				return false, err
			}
		} else {
			concurentConn = 1
			fmt.Println("Accept-Ranges not supported")
			download()
		}
	}

	if (time.Since(begin)) >= 30*time.Second {
		return false, errors.New("Download timed out!")
	}

	if downloadCompleted != concurentConn {
		return false, errors.New("Download error :(")
	}

	return true, nil
}

func main() {
	completed, err := Run() //returns bool and error, can be used in conditional statements
	if completed {
		fmt.Println("Download Completed :D")
	} else {
		fmt.Println(err)
	}
}
