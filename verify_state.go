package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func testStateSaving() {
	// Test the state saving functionality
	url := "https://www.w3.org/WAI/ER/tests/xhtml/testfiles/resources/pdf/dummy.pdf"
	
	downloader := NewDownloader(url, 8, 2*1024*1024, "", 0, true, 10, time.Second, 1024*1024*1024)
	downloader.totalSize = 13264
	downloader.downloaded.Store(5000)
	downloader.bytePosition.Store(5000)
	
	// Create a test file and buffer
	tmpFile, err := os.CreateTemp("", "test-*.pdf")
	if err != nil {
		fmt.Printf("Error creating temp file: %v\n", err)
		return
	}
	defer os.Remove(tmpFile.Name())
	
	downloader.currentFile = tmpFile
	downloader.stateFile = getStateFilename(url)
	
	fmt.Printf("State file will be: %s\n", downloader.stateFile)
	
	// Test GracefulShutdown
	if err := downloader.GracefulShutdown(); err != nil {
		fmt.Printf("Error in graceful shutdown: %v\n", err)
		return
	}
	
	// Read and display the state file
	data, err := os.ReadFile(downloader.stateFile)
	if err != nil {
		fmt.Printf("Error reading state file: %v\n", err)
		return
	}
	
	fmt.Println("\nState file contents:")
	var prettyJSON map[string]interface{}
	json.Unmarshal(data, &prettyJSON)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.Encode(prettyJSON)
	
	// Clean up
	os.Remove(downloader.stateFile)
	fmt.Println("\nState saving test successful!")
}