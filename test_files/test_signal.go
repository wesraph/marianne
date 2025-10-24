package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	url := "https://www.w3.org/WAI/ER/tests/xhtml/testfiles/resources/pdf/dummy.pdf"
	
	// Set up signal handler
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
	
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	// Start download
	go func() {
		fmt.Println("Starting download...")
		
		resp, err := http.Get(url)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		defer resp.Body.Close()
		
		fmt.Printf("Content-Length: %d\n", resp.ContentLength)
		
		file, err := os.Create("test-download.pdf")
		if err != nil {
			fmt.Printf("Error creating file: %v\n", err)
			return
		}
		defer file.Close()
		
		// Copy with context
		reader := resp.Body
		buffer := make([]byte, 1024)
		totalBytes := int64(0)
		
		for {
			select {
			case <-ctx.Done():
				fmt.Printf("\nDownload cancelled, downloaded %d bytes\n", totalBytes)
				
				// Save state
				stateFile := ".test-state.json"
				state := fmt.Sprintf(`{"bytes_downloaded": %d, "url": "%s"}`, totalBytes, url)
				os.WriteFile(stateFile, []byte(state), 0644)
				fmt.Printf("State saved to %s\n", stateFile)
				return
			default:
				n, err := reader.Read(buffer)
				if n > 0 {
					file.Write(buffer[:n])
					totalBytes += int64(n)
					fmt.Printf("\rDownloaded: %d bytes", totalBytes)
				}
				if err == io.EOF {
					fmt.Printf("\nDownload complete: %d bytes\n", totalBytes)
					return
				}
				if err != nil {
					fmt.Printf("\nError reading: %v\n", err)
					return
				}
				
				// Slow down for testing
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	
	// Wait for signal
	sig := <-sigChan
	fmt.Printf("\nReceived signal: %v\n", sig)
	cancel()
	
	// Give goroutine time to clean up
	time.Sleep(1 * time.Second)
}