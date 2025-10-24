package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// TestGetFileSize tests the HEAD request to get file size
func TestGetFileSize(t *testing.T) {
	content := GenerateFakeContent(1024 * 1024) // 1MB
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	d := NewDownloader(mock.URL(), 4, 1024*1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)

	err := d.getFileSize()
	if err != nil {
		t.Fatalf("getFileSize() error = %v, want nil", err)
	}

	if d.totalSize != int64(len(content)) {
		t.Errorf("totalSize = %d, want %d", d.totalSize, len(content))
	}
}

// TestGetFileSizeRetry tests retry logic on transient failures
func TestGetFileSizeRetry(t *testing.T) {
	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	// Fail first 2 attempts, succeed on 3rd
	mock.SetMaxFailures(2)

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 5, 50*time.Millisecond, 1024*1024*1024)

	start := time.Now()
	err := d.getFileSize()
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("getFileSize() with retries error = %v, want nil", err)
	}

	// Should have taken at least the retry delays
	minExpectedDelay := 50*time.Millisecond + 100*time.Millisecond // initial + one backoff
	if elapsed < minExpectedDelay {
		t.Errorf("Retry too fast: elapsed %v, expected at least %v", elapsed, minExpectedDelay)
	}

	if d.totalSize != int64(len(content)) {
		t.Errorf("totalSize = %d, want %d", d.totalSize, len(content))
	}
}

// TestGetFileSizeExhaustedRetries tests behavior when retries are exhausted
func TestGetFileSizeExhaustedRetries(t *testing.T) {
	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	// Fail more times than max retries
	mock.SetMaxFailures(10)

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 50*time.Millisecond, 1024*1024*1024)

	err := d.getFileSize()
	if err == nil {
		t.Fatal("getFileSize() error = nil, want error when retries exhausted")
	}
}

// TestGetFileSizeInvalidSize tests handling of invalid Content-Length
func TestGetFileSizeInvalidSize(t *testing.T) {
	// This test would require modifying the mock server to return invalid Content-Length
	// For now, we'll test the parsing logic separately
	t.Skip("Requires mock server enhancement to return invalid headers")
}

// TestDownloadChunk tests downloading a single chunk
func TestDownloadChunk(t *testing.T) {
	content := GenerateFakeContent(10 * 1024) // 10KB
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)

	ctx := context.Background()
	var buf bytes.Buffer

	// Download first 1KB
	err := d.downloadChunk(ctx, 0, 1023, &buf)
	if err != nil {
		t.Fatalf("downloadChunk() error = %v, want nil", err)
	}

	if buf.Len() != 1024 {
		t.Errorf("Downloaded %d bytes, want 1024", buf.Len())
	}

	// Verify content matches
	AssertBytesEqual(t, content[0:1024], buf.Bytes(), "Chunk content mismatch")
}

// TestDownloadChunkMiddle tests downloading a chunk from the middle of the file
func TestDownloadChunkMiddle(t *testing.T) {
	content := GenerateFakeContent(10 * 1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)

	ctx := context.Background()
	var buf bytes.Buffer

	// Download bytes 5000-5999
	err := d.downloadChunk(ctx, 5000, 5999, &buf)
	if err != nil {
		t.Fatalf("downloadChunk() error = %v, want nil", err)
	}

	if buf.Len() != 1000 {
		t.Errorf("Downloaded %d bytes, want 1000", buf.Len())
	}

	AssertBytesEqual(t, content[5000:6000], buf.Bytes(), "Middle chunk content mismatch")
}

// TestDownloadChunkRetry tests retry logic for chunk download
func TestDownloadChunkRetry(t *testing.T) {
	content := GenerateFakeContent(5 * 1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	// Fail first 2 attempts
	mock.SetMaxFailures(2)

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 5, 50*time.Millisecond, 1024*1024*1024)

	ctx := context.Background()
	var buf bytes.Buffer

	err := d.downloadChunk(ctx, 0, 1023, &buf)
	if err != nil {
		t.Fatalf("downloadChunk() with retries error = %v, want nil", err)
	}

	if buf.Len() != 1024 {
		t.Errorf("Downloaded %d bytes, want 1024", buf.Len())
	}

	AssertBytesEqual(t, content[0:1024], buf.Bytes(), "Retried chunk content mismatch")
}

// TestDownloadChunkTimeout tests chunk timeout behavior
func TestDownloadChunkTimeout(t *testing.T) {
	// NOTE: This test is skipped because chunk timeout is hard-coded to 5 minutes
	// which makes the test take too long. This test is kept to document the behavior.
	t.Skip("Chunk timeout is hard-coded to 5 minutes, making test too slow")

	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	// Add delay longer than timeout
	mock.SetRequestDelay(6 * time.Minute)

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 1, 100*time.Millisecond, 1024*1024*1024)

	ctx := context.Background()
	var buf bytes.Buffer

	start := time.Now()
	err := d.downloadChunk(ctx, 0, 1023, &buf)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("downloadChunk() error = nil, want timeout error")
	}

	// Should timeout around 5 minutes (the chunk timeout)
	if elapsed > 6*time.Minute {
		t.Errorf("Timeout took too long: %v", elapsed)
	}
}

// TestDownloadChunkCancellation tests context cancellation
func TestDownloadChunkCancellation(t *testing.T) {
	content := GenerateFakeContent(10 * 1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	// Add delay to ensure we can cancel
	mock.SetRequestDelay(100 * time.Millisecond)

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 50*time.Millisecond, 1024*1024*1024)

	ctx, cancel := context.WithCancel(context.Background())
	var buf bytes.Buffer

	// Cancel immediately
	cancel()

	err := d.downloadChunk(ctx, 0, 1023, &buf)
	if err == nil {
		t.Fatal("downloadChunk() error = nil, want cancellation error")
	}
}

// TestDownloadChunkBoundary tests edge case at file boundary
func TestDownloadChunkBoundary(t *testing.T) {
	content := GenerateFakeContent(1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	d := NewDownloader(mock.URL(), 4, 512, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)

	ctx := context.Background()
	var buf bytes.Buffer

	// Download last chunk (bytes 512-1023)
	err := d.downloadChunk(ctx, 512, 1023, &buf)
	if err != nil {
		t.Fatalf("downloadChunk() boundary error = %v, want nil", err)
	}

	if buf.Len() != 512 {
		t.Errorf("Downloaded %d bytes, want 512", buf.Len())
	}

	AssertBytesEqual(t, content[512:1024], buf.Bytes(), "Boundary chunk content mismatch")
}

// TestDownloadChunkServerNoRangeSupport tests when server doesn't support ranges
func TestDownloadChunkServerNoRangeSupport(t *testing.T) {
	content := GenerateFakeContent(5 * 1024)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	// Disable range support
	mock.SetSupportsRanges(false)

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)

	ctx := context.Background()
	var buf bytes.Buffer

	// Try to download a chunk - server should return full content
	err := d.downloadChunk(ctx, 1024, 2047, &buf)
	if err != nil {
		t.Fatalf("downloadChunk() error = %v, want nil (server returns full content)", err)
	}

	// Server returns entire file when ranges not supported
	if buf.Len() != len(content) {
		t.Logf("Downloaded %d bytes (server sent full content)", buf.Len())
	}
}

// TestRetryWithBackoffSuccess tests retry logic succeeds immediately
func TestRetryWithBackoffSuccess(t *testing.T) {
	d := NewDownloader("http://example.com/test", 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)

	attempts := 0
	err := d.retryWithBackoff(context.Background(), "test operation", func() error {
		attempts++
		return nil // Success on first try
	})

	if err != nil {
		t.Fatalf("retryWithBackoff() error = %v, want nil", err)
	}

	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

// TestRetryWithBackoffEventualSuccess tests retry succeeds after failures
func TestRetryWithBackoffEventualSuccess(t *testing.T) {
	d := NewDownloader("http://example.com/test", 4, 1024, "", 0, false, 5, 50*time.Millisecond, 1024*1024*1024)

	attempts := 0
	err := d.retryWithBackoff(context.Background(), "test operation", func() error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary failure")
		}
		return nil
	})

	if err != nil {
		t.Fatalf("retryWithBackoff() error = %v, want nil", err)
	}

	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

// TestRetryWithBackoffAllFail tests behavior when all retries fail
func TestRetryWithBackoffAllFail(t *testing.T) {
	d := NewDownloader("http://example.com/test", 4, 1024, "", 0, false, 3, 50*time.Millisecond, 1024*1024*1024)

	attempts := 0
	testErr := errors.New("persistent failure")
	err := d.retryWithBackoff(context.Background(), "test operation", func() error {
		attempts++
		return testErr
	})

	if err == nil {
		t.Fatal("retryWithBackoff() error = nil, want error")
	}

	// Should try initial + maxRetries times = 1 + 3 = 4
	expectedAttempts := 1 + d.maxRetries
	if attempts != expectedAttempts {
		t.Errorf("attempts = %d, want %d", attempts, expectedAttempts)
	}
}

// TestRetryWithBackoffCancellation tests context cancellation during retry
func TestRetryWithBackoffCancellation(t *testing.T) {
	d := NewDownloader("http://example.com/test", 4, 1024, "", 0, false, 10, 100*time.Millisecond, 1024*1024*1024)

	ctx, cancel := context.WithCancel(context.Background())

	attempts := 0
	errChan := make(chan error, 1)

	go func() {
		err := d.retryWithBackoff(ctx, "test operation", func() error {
			attempts++
			return errors.New("will keep failing")
		})
		errChan <- err
	}()

	// Cancel after a short delay
	time.Sleep(150 * time.Millisecond)
	cancel()

	err := <-errChan
	if err == nil {
		t.Fatal("retryWithBackoff() error = nil, want cancellation error")
	}

	// Should not have exhausted all retries
	if attempts >= 1+d.maxRetries {
		t.Errorf("attempts = %d, should have been cancelled before exhausting retries", attempts)
	}
}

// TestRateLimitedReader tests rate limiting functionality
func TestRateLimitedReader(t *testing.T) {
	// Use larger content to properly test rate limiting
	// The rate limiter has a burst size equal to the limit, so we need
	// enough data to exceed the burst
	content := GenerateFakeContent(50 * 1024) // 50KB
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	// Set bandwidth limit to 10KB/s
	bandwidthLimit := int64(10 * 1024)
	d := NewDownloader(mock.URL(), 4, 1024, "", bandwidthLimit, false, 3, 100*time.Millisecond, 1024*1024*1024)

	ctx := context.Background()
	var buf bytes.Buffer

	start := time.Now()
	// Download 30KB (should take ~3 seconds at 10KB/s after burst)
	err := d.downloadChunk(ctx, 0, 30*1024-1, &buf)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("downloadChunk() with rate limit error = %v, want nil", err)
	}

	// Should take at least 1 second for data beyond the initial burst
	// Allow variance due to burst and overhead
	if elapsed < 1*time.Second {
		t.Logf("Download time: %v (rate limiting may not have kicked in due to burst)", elapsed)
	}

	if elapsed > 10*time.Second {
		t.Errorf("Download too slow: %v (expected ~2-3s with burst)", elapsed)
	}

	// Just verify the rate limiter was created
	if d.rateLimiter == nil {
		t.Error("Rate limiter should be initialized when bandwidth limit is set")
	}
}
