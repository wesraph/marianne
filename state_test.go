package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

// TestSaveState tests state serialization
func TestSaveState(t *testing.T) {
	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d.totalSize = int64(len(content))
	d.downloaded.Store(512)
	d.bytePosition.Store(512)
	d.stateFile = ".test-state-save.json"
	defer os.Remove(d.stateFile)

	// Initialize state
	d.state = &DownloadState{
		URL:       d.url,
		TotalSize: d.totalSize,
		Created:   time.Now(),
	}

	err := d.saveState()
	if err != nil {
		t.Fatalf("saveState() error = %v, want nil", err)
	}

	// Verify file exists
	if _, err := os.Stat(d.stateFile); os.IsNotExist(err) {
		t.Fatalf("State file not created: %s", d.stateFile)
	}

	// Read and verify contents
	data, err := os.ReadFile(d.stateFile)
	if err != nil {
		t.Fatalf("Failed to read state file: %v", err)
	}

	var state DownloadState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("Failed to unmarshal state: %v", err)
	}

	if state.URL != d.url {
		t.Errorf("URL = %s, want %s", state.URL, d.url)
	}
	if state.TotalSize != d.totalSize {
		t.Errorf("TotalSize = %d, want %d", state.TotalSize, d.totalSize)
	}
	if state.Downloaded != 512 {
		t.Errorf("Downloaded = %d, want 512", state.Downloaded)
	}
	if state.BytePosition != 512 {
		t.Errorf("BytePosition = %d, want 512", state.BytePosition)
	}
}

// TestLoadState tests state deserialization
func TestLoadState(t *testing.T) {
	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	// Create a state file
	stateFile := ".test-state-load.json"
	defer os.Remove(stateFile)

	state := DownloadState{
		URL:          mock.URL(),
		TotalSize:    1024,
		Downloaded:   512,
		BytePosition: 512,
		Created:      time.Now(),
		Updated:      time.Now(),
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal state: %v", err)
	}

	if err := os.WriteFile(stateFile, data, 0644); err != nil {
		t.Fatalf("Failed to write state file: %v", err)
	}

	// Load the state
	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d.stateFile = stateFile

	err = d.loadState()
	if err != nil {
		t.Fatalf("loadState() error = %v, want nil", err)
	}

	if d.state.URL != mock.URL() {
		t.Errorf("Loaded URL = %s, want %s", d.state.URL, mock.URL())
	}
	if d.state.TotalSize != 1024 {
		t.Errorf("Loaded TotalSize = %d, want 1024", d.state.TotalSize)
	}
	if d.state.Downloaded != 512 {
		t.Errorf("Loaded Downloaded = %d, want 512", d.state.Downloaded)
	}
	if d.downloaded.Load() != 512 {
		t.Errorf("Loaded downloaded counter = %d, want 512", d.downloaded.Load())
	}
}

// TestLoadStateURLMismatch tests URL validation
func TestLoadStateURLMismatch(t *testing.T) {
	// Create a state file with different URL
	stateFile := ".test-state-mismatch.json"
	defer os.Remove(stateFile)

	state := DownloadState{
		URL:       "https://different.com/file.tar.gz",
		TotalSize: 1024,
		Created:   time.Now(),
		Updated:   time.Now(),
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal state: %v", err)
	}

	if err := os.WriteFile(stateFile, data, 0644); err != nil {
		t.Fatalf("Failed to write state file: %v", err)
	}

	// Try to load with different URL
	d := NewDownloader("https://example.com/file.tar.gz", 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d.stateFile = stateFile

	err = d.loadState()
	if err == nil {
		t.Fatal("loadState() error = nil, want URL mismatch error")
	}
}

// TestLoadStateInvalidJSON tests error handling for corrupt state files
func TestLoadStateInvalidJSON(t *testing.T) {
	stateFile := ".test-state-invalid.json"
	defer os.Remove(stateFile)

	// Write invalid JSON
	if err := os.WriteFile(stateFile, []byte("{invalid json"), 0644); err != nil {
		t.Fatalf("Failed to write invalid state file: %v", err)
	}

	d := NewDownloader("https://example.com/file.tar.gz", 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d.stateFile = stateFile

	err := d.loadState()
	if err == nil {
		t.Fatal("loadState() error = nil, want JSON unmarshal error")
	}
}

// TestLoadStateNonExistent tests loading from non-existent file
func TestLoadStateNonExistent(t *testing.T) {
	d := NewDownloader("https://example.com/file.tar.gz", 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d.stateFile = ".test-state-nonexistent.json"

	err := d.loadState()
	if err == nil {
		t.Fatal("loadState() error = nil, want file not found error")
	}
}

// TestCleanupState tests state file cleanup
func TestCleanupState(t *testing.T) {
	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d.stateFile = ".test-state-cleanup.json"
	d.state = &DownloadState{
		URL:       d.url,
		TotalSize: 1024,
		Created:   time.Now(),
	}

	// Save state
	if err := d.saveState(); err != nil {
		t.Fatalf("saveState() failed: %v", err)
	}

	// Verify it exists
	if _, err := os.Stat(d.stateFile); os.IsNotExist(err) {
		t.Fatal("State file should exist before cleanup")
	}

	// Cleanup
	d.cleanupState()

	// Verify it's deleted
	if _, err := os.Stat(d.stateFile); !os.IsNotExist(err) {
		t.Error("State file should be deleted after cleanup")
	}
}

// TestGracefulShutdown tests graceful shutdown behavior
func TestGracefulShutdown(t *testing.T) {
	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	// Create a temp file for testing
	tmpFile, err := os.CreateTemp("", "test-shutdown-*.dat")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d.stateFile = ".test-state-shutdown.json"
	defer os.Remove(d.stateFile)

	d.totalSize = 1024
	d.downloaded.Store(512)
	d.bytePosition.Store(512)
	d.currentFile = tmpFile
	d.writeBuffer = bufio.NewWriter(tmpFile)

	// Write some data to buffer
	testData := "test data"
	d.writeBuffer.WriteString(testData)

	// Initialize state
	d.state = &DownloadState{
		URL:       d.url,
		TotalSize: d.totalSize,
		Created:   time.Now(),
	}

	// Call graceful shutdown
	err = d.GracefulShutdown()
	if err != nil {
		t.Fatalf("GracefulShutdown() error = %v, want nil", err)
	}

	// Verify state was saved
	if _, err := os.Stat(d.stateFile); os.IsNotExist(err) {
		t.Fatal("State file should exist after graceful shutdown")
	}

	// Verify shutdown channel is closed
	select {
	case <-d.shutdown:
		// Expected
	case <-time.After(100 * time.Millisecond):
		t.Error("Shutdown channel should be closed")
	}

	// Read state and verify byte position
	data, err := os.ReadFile(d.stateFile)
	if err != nil {
		t.Fatalf("Failed to read state file: %v", err)
	}

	var state DownloadState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("Failed to unmarshal state: %v", err)
	}

	// BytePosition should match the actual file position after flush
	// which is len(testData) since we started from the beginning
	expectedPos := int64(len(testData))
	if state.BytePosition != expectedPos {
		t.Errorf("BytePosition = %d, want %d", state.BytePosition, expectedPos)
	}
}

// TestGracefulShutdownWithoutFile tests shutdown without current file
func TestGracefulShutdownWithoutFile(t *testing.T) {
	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d.stateFile = ".test-state-no-file.json"
	defer os.Remove(d.stateFile)

	d.totalSize = 1024
	d.state = &DownloadState{
		URL:       d.url,
		TotalSize: d.totalSize,
		Created:   time.Now(),
	}

	// Shutdown without current file
	err := d.GracefulShutdown()
	if err != nil {
		t.Fatalf("GracefulShutdown() without file error = %v, want nil", err)
	}

	// Should still save state
	if _, err := os.Stat(d.stateFile); os.IsNotExist(err) {
		t.Fatal("State file should exist even without current file")
	}
}

// TestStateRace tests concurrent state saves (RACE CONDITION TEST)
func TestStateRace(t *testing.T) {
	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d.stateFile = ".test-state-race.json"
	defer os.Remove(d.stateFile)

	d.totalSize = 1024
	d.state = &DownloadState{
		URL:       d.url,
		TotalSize: d.totalSize,
		Created:   time.Now(),
	}

	// Concurrent saves
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			d.downloaded.Store(int64(id * 100))
			d.bytePosition.Store(int64(id * 100))
			d.saveState() // This should be safe due to mutex
		}(i)
	}

	wg.Wait()

	// Verify state file is valid JSON
	data, err := os.ReadFile(d.stateFile)
	if err != nil {
		t.Fatalf("Failed to read state file after concurrent saves: %v", err)
	}

	var state DownloadState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("State file corrupted after concurrent saves: %v", err)
	}
}

// TestStateUpdateTracking tests that state updates are tracked
func TestStateUpdateTracking(t *testing.T) {
	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d.stateFile = ".test-state-tracking.json"
	defer os.Remove(d.stateFile)

	d.totalSize = 1024
	d.state = &DownloadState{
		URL:       d.url,
		TotalSize: d.totalSize,
		Created:   time.Now(),
	}

	// Save initially
	err := d.saveState()
	if err != nil {
		t.Fatalf("saveState() error = %v", err)
	}

	firstUpdate := d.state.Updated

	// Wait a bit
	time.Sleep(100 * time.Millisecond)

	// Update and save again
	d.downloaded.Store(512)
	err = d.saveState()
	if err != nil {
		t.Fatalf("saveState() second time error = %v", err)
	}

	secondUpdate := d.state.Updated

	// Updated timestamp should be different
	if !secondUpdate.After(firstUpdate) {
		t.Errorf("Updated timestamp should be later after second save")
	}
}

// TestStateWithExtractedFiles tests state with extracted files list
func TestStateWithExtractedFiles(t *testing.T) {
	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d.stateFile = ".test-state-files.json"
	defer os.Remove(d.stateFile)

	d.totalSize = 1024
	d.state = &DownloadState{
		URL:            d.url,
		TotalSize:      d.totalSize,
		ExtractedFiles: []string{"file1.txt", "file2.txt", "file3.txt"},
		Created:        time.Now(),
	}

	err := d.saveState()
	if err != nil {
		t.Fatalf("saveState() with files error = %v", err)
	}

	// Load and verify
	d2 := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d2.stateFile = d.stateFile

	err = d2.loadState()
	if err != nil {
		t.Fatalf("loadState() error = %v", err)
	}

	if len(d2.state.ExtractedFiles) != 3 {
		t.Errorf("ExtractedFiles count = %d, want 3", len(d2.state.ExtractedFiles))
	}
}

// TestStateWithETag tests ETag preservation
func TestStateWithETag(t *testing.T) {
	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	mock.SetETag(`"test-etag-456"`)

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d.stateFile = ".test-state-etag.json"
	defer os.Remove(d.stateFile)

	// Get file size (which sets ETag)
	err := d.getFileSize()
	if err != nil {
		t.Fatalf("getFileSize() error = %v", err)
	}

	// Save state
	err = d.saveState()
	if err != nil {
		t.Fatalf("saveState() error = %v", err)
	}

	// Load and verify ETag
	d2 := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d2.stateFile = d.stateFile

	err = d2.loadState()
	if err != nil {
		t.Fatalf("loadState() error = %v", err)
	}

	if d2.state.ETag != `"test-etag-456"` {
		t.Errorf("ETag = %s, want %s", d2.state.ETag, `"test-etag-456"`)
	}
}

// TestGracefulShutdownTwice tests calling graceful shutdown multiple times
func TestGracefulShutdownTwice(t *testing.T) {
	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)
	d.stateFile = ".test-state-twice.json"
	defer os.Remove(d.stateFile)

	d.totalSize = 1024
	d.state = &DownloadState{
		URL:       d.url,
		TotalSize: d.totalSize,
		Created:   time.Now(),
	}

	// First shutdown
	err := d.GracefulShutdown()
	if err != nil {
		t.Fatalf("First GracefulShutdown() error = %v", err)
	}

	// Second shutdown should NOT panic (FIXED with sync.Once)
	// This should be safe now
	err = d.GracefulShutdown()
	if err != nil {
		t.Fatalf("Second GracefulShutdown() error = %v", err)
	}

	// Verify no panic occurred
	t.Log("✅ FIXED: Double GracefulShutdown() no longer panics (protected by sync.Once)")
}
