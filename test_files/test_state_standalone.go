package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type TestState struct {
	URL          string    `json:"url"`
	TotalSize    int64     `json:"total_size"`
	Downloaded   int64     `json:"downloaded"`
	BytePosition int64     `json:"byte_position"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
}

func main() {
	fmt.Println("Testing state file creation...")
	
	state := TestState{
		URL:          "https://www.w3.org/WAI/ER/tests/xhtml/testfiles/resources/pdf/dummy.pdf",
		TotalSize:    13264,
		Downloaded:   5000,
		BytePosition: 5000,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	
	stateFile := ".marianne-test-state.json"
	
	// Save state
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		fmt.Printf("Error marshaling state: %v\n", err)
		return
	}
	
	if err := os.WriteFile(stateFile, data, 0644); err != nil {
		fmt.Printf("Error writing state file: %v\n", err)
		return
	}
	
	fmt.Printf("State file created: %s\n", stateFile)
	fmt.Println("\nContents:")
	fmt.Println(string(data))
	
	// Clean up
	os.Remove(stateFile)
	fmt.Println("\nTest successful!")
}