package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// MockHTTPServer provides a configurable mock HTTP server for testing
type MockHTTPServer struct {
	server         *httptest.Server
	content        []byte
	contentType    string
	supportsRanges bool
	etag           string
	lastModified   string
	failureCount   int32 // atomic counter for simulating transient failures
	maxFailures    int32
	requestDelay   time.Duration
	statusCode     int
}

// NewMockHTTPServer creates a new mock HTTP server with the given content
func NewMockHTTPServer(content []byte) *MockHTTPServer {
	m := &MockHTTPServer{
		content:        content,
		contentType:    "application/octet-stream",
		supportsRanges: true,
		etag:           `"test-etag-123"`,
		lastModified:   time.Now().Format(http.TimeFormat),
		statusCode:     http.StatusOK,
	}

	m.server = httptest.NewServer(http.HandlerFunc(m.handler))
	return m
}

// URL returns the mock server URL
func (m *MockHTTPServer) URL() string {
	return m.server.URL + "/test-file.tar.gz"
}

// Close closes the mock server
func (m *MockHTTPServer) Close() {
	m.server.Close()
}

// SetMaxFailures sets how many times requests should fail before succeeding
func (m *MockHTTPServer) SetMaxFailures(count int) {
	atomic.StoreInt32(&m.maxFailures, int32(count))
	atomic.StoreInt32(&m.failureCount, 0)
}

// SetRequestDelay sets a delay for each request
func (m *MockHTTPServer) SetRequestDelay(delay time.Duration) {
	m.requestDelay = delay
}

// SetSupportsRanges configures whether the server supports range requests
func (m *MockHTTPServer) SetSupportsRanges(supports bool) {
	m.supportsRanges = supports
}

// SetETag sets the ETag header
func (m *MockHTTPServer) SetETag(etag string) {
	m.etag = etag
}

// SetStatusCode sets the HTTP status code to return
func (m *MockHTTPServer) SetStatusCode(code int) {
	m.statusCode = code
}

func (m *MockHTTPServer) handler(w http.ResponseWriter, r *http.Request) {
	// Simulate delay if configured
	if m.requestDelay > 0 {
		time.Sleep(m.requestDelay)
	}

	// Simulate transient failures
	currentFailures := atomic.LoadInt32(&m.failureCount)
	maxFailures := atomic.LoadInt32(&m.maxFailures)
	if currentFailures < maxFailures {
		atomic.AddInt32(&m.failureCount, 1)
		http.Error(w, "Simulated transient failure", http.StatusServiceUnavailable)
		return
	}

	// Handle HEAD requests
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(m.content)))
		w.Header().Set("Content-Type", m.contentType)
		if m.supportsRanges {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		if m.etag != "" {
			w.Header().Set("ETag", m.etag)
		}
		if m.lastModified != "" {
			w.Header().Set("Last-Modified", m.lastModified)
		}
		w.WriteHeader(m.statusCode)
		return
	}

	// Handle GET requests
	if r.Method == http.MethodGet {
		rangeHeader := r.Header.Get("Range")

		if rangeHeader != "" && m.supportsRanges {
			// Parse range header (bytes=start-end)
			var start, end int64
			_, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
			if err != nil {
				http.Error(w, "Invalid range", http.StatusBadRequest)
				return
			}

			// Validate range
			if start < 0 || start >= int64(len(m.content)) {
				http.Error(w, "Range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
				return
			}

			if end >= int64(len(m.content)) {
				end = int64(len(m.content)) - 1
			}

			// Send partial content
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(m.content)))
			w.Header().Set("Content-Type", m.contentType)
			if m.etag != "" {
				w.Header().Set("ETag", m.etag)
			}
			if m.lastModified != "" {
				w.Header().Set("Last-Modified", m.lastModified)
			}
			w.WriteHeader(http.StatusPartialContent)
			w.Write(m.content[start : end+1])
		} else {
			// Send full content
			w.Header().Set("Content-Length", strconv.Itoa(len(m.content)))
			w.Header().Set("Content-Type", m.contentType)
			if m.supportsRanges {
				w.Header().Set("Accept-Ranges", "bytes")
			}
			if m.etag != "" {
				w.Header().Set("ETag", m.etag)
			}
			if m.lastModified != "" {
				w.Header().Set("Last-Modified", m.lastModified)
			}
			w.WriteHeader(m.statusCode)
			w.Write(m.content)
		}
	}
}

// GenerateFakeContent generates fake binary content of the specified size
func GenerateFakeContent(size int) []byte {
	content := make([]byte, size)
	for i := 0; i < size; i++ {
		content[i] = byte(i % 256)
	}
	return content
}

// GenerateFakeTarGz creates a fake .tar.gz archive with test files
func GenerateFakeTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzWriter)

	for filename, content := range files {
		header := &tar.Header{
			Name:    filename,
			Size:    int64(len(content)),
			Mode:    0644,
			ModTime: time.Now(),
		}

		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("Failed to write tar header: %v", err)
		}

		if _, err := tarWriter.Write(content); err != nil {
			t.Fatalf("Failed to write tar content: %v", err)
		}
	}

	if err := tarWriter.Close(); err != nil {
		t.Fatalf("Failed to close tar writer: %v", err)
	}

	if err := gzWriter.Close(); err != nil {
		t.Fatalf("Failed to close gzip writer: %v", err)
	}

	return buf.Bytes()
}

// GenerateFakeZip creates a fake .zip archive with test files
func GenerateFakeZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zipWriter := zip.NewWriter(&buf)

	for filename, content := range files {
		fileWriter, err := zipWriter.Create(filename)
		if err != nil {
			t.Fatalf("Failed to create zip entry: %v", err)
		}

		if _, err := fileWriter.Write(content); err != nil {
			t.Fatalf("Failed to write zip content: %v", err)
		}
	}

	if err := zipWriter.Close(); err != nil {
		t.Fatalf("Failed to close zip writer: %v", err)
	}

	return buf.Bytes()
}

// GenerateMaliciousZip creates a ZIP with path traversal attempts
func GenerateMaliciousZip(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	zipWriter := zip.NewWriter(&buf)

	// Try various path traversal attacks
	maliciousPaths := []string{
		"../../../etc/passwd",
		"..\\..\\..\\windows\\system32\\config\\sam",
		"/etc/shadow",
		"safe-file.txt",
		"../../escaping.txt",
	}

	for _, path := range maliciousPaths {
		fileWriter, err := zipWriter.Create(path)
		if err != nil {
			t.Fatalf("Failed to create malicious zip entry: %v", err)
		}

		content := []byte("MALICIOUS CONTENT")
		if _, err := fileWriter.Write(content); err != nil {
			t.Fatalf("Failed to write malicious content: %v", err)
		}
	}

	if err := zipWriter.Close(); err != nil {
		t.Fatalf("Failed to close malicious zip: %v", err)
	}

	return buf.Bytes()
}

// GenerateLargeZip creates a ZIP with many files to test file descriptor limits
func GenerateLargeZip(t *testing.T, fileCount int) []byte {
	t.Helper()

	var buf bytes.Buffer
	zipWriter := zip.NewWriter(&buf)

	for i := 0; i < fileCount; i++ {
		filename := fmt.Sprintf("file_%06d.txt", i)
		fileWriter, err := zipWriter.Create(filename)
		if err != nil {
			t.Fatalf("Failed to create zip entry %d: %v", i, err)
		}

		content := []byte(fmt.Sprintf("Content of file %d\n", i))
		if _, err := fileWriter.Write(content); err != nil {
			t.Fatalf("Failed to write zip content %d: %v", i, err)
		}
	}

	if err := zipWriter.Close(); err != nil {
		t.Fatalf("Failed to close large zip: %v", err)
	}

	return buf.Bytes()
}

// AssertBytesEqual checks if two byte slices are equal
func AssertBytesEqual(t *testing.T, expected, actual []byte, msg string) {
	t.Helper()
	if !bytes.Equal(expected, actual) {
		t.Errorf("%s: expected %d bytes, got %d bytes", msg, len(expected), len(actual))
		if len(expected) < 100 && len(actual) < 100 {
			t.Errorf("Expected: %v", expected)
			t.Errorf("Actual: %v", actual)
		}
	}
}

// AssertError checks if an error occurred when expected
func AssertError(t *testing.T, err error, expectedError bool, msg string) {
	t.Helper()
	if expectedError && err == nil {
		t.Errorf("%s: expected error but got nil", msg)
	}
	if !expectedError && err != nil {
		t.Errorf("%s: unexpected error: %v", msg, err)
	}
}

// AssertInt64Equal checks if two int64 values are equal
func AssertInt64Equal(t *testing.T, expected, actual int64, msg string) {
	t.Helper()
	if expected != actual {
		t.Errorf("%s: expected %d, got %d", msg, expected, actual)
	}
}

// AssertStringEqual checks if two strings are equal
func AssertStringEqual(t *testing.T, expected, actual string, msg string) {
	t.Helper()
	if expected != actual {
		t.Errorf("%s: expected %q, got %q", msg, expected, actual)
	}
}

// AssertTrue checks if a condition is true
func AssertTrue(t *testing.T, condition bool, msg string) {
	t.Helper()
	if !condition {
		t.Errorf("%s: expected true, got false", msg)
	}
}

// AssertFalse checks if a condition is false
func AssertFalse(t *testing.T, condition bool, msg string) {
	t.Helper()
	if condition {
		t.Errorf("%s: expected false, got true", msg)
	}
}

// SlowReader wraps an io.Reader and adds artificial delay
type SlowReader struct {
	reader io.Reader
	delay  time.Duration
}

func NewSlowReader(reader io.Reader, delay time.Duration) *SlowReader {
	return &SlowReader{reader: reader, delay: delay}
}

func (s *SlowReader) Read(p []byte) (int, error) {
	time.Sleep(s.delay)
	return s.reader.Read(p)
}

// FailingReader simulates read failures
type FailingReader struct {
	reader       io.Reader
	failAfter    int
	failCount    int
	currentReads int
	err          error
}

func NewFailingReader(reader io.Reader, failAfter int, failCount int, err error) *FailingReader {
	return &FailingReader{
		reader:    reader,
		failAfter: failAfter,
		failCount: failCount,
		err:       err,
	}
}

func (f *FailingReader) Read(p []byte) (int, error) {
	f.currentReads++

	if f.currentReads > f.failAfter && f.currentReads <= f.failAfter+f.failCount {
		return 0, f.err
	}

	return f.reader.Read(p)
}
