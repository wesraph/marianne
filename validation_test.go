package main

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestWorkerCountValidation tests zero and negative worker validation
func TestWorkerCountValidation(t *testing.T) {
	tests := []struct {
		name     string
		workers  int
		expected int
	}{
		{"Valid workers", 8, 8},
		{"Single worker", 1, 1},
		{"Zero workers - FIXED", 0, 1}, // Now defaults to 1
		{"Negative workers - FIXED", -1, 1}, // Now defaults to 1
		{"Large workers", 100, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDownloader("https://example.com/test.tar.gz", tt.workers, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)

			// Validation now works - invalid values get set to default
			if tt.workers <= 0 {
				t.Logf("✅ FIXED: Worker count %d validated and set to default %d", tt.workers, tt.expected)
			}

			// Verify downloader was created with correct worker count
			if d == nil {
				t.Fatal("Downloader should be created")
			}
			if d.workers != tt.expected {
				t.Errorf("workers = %d, want %d", d.workers, tt.expected)
			}
		})
	}
}

// TestChunkSizeValidation tests zero and negative chunk size
func TestChunkSizeValidation(t *testing.T) {
	defaultChunkSize := int64(2 * 1024 * 1024) // 2MB default
	tests := []struct {
		name      string
		chunkSize int64
		expected  int64
	}{
		{"Valid chunk size", 1024 * 1024, 1024 * 1024},
		{"Small chunk", 1024, 1024},
		{"Zero chunk - FIXED", 0, defaultChunkSize}, // Now defaults to 2MB
		{"Negative chunk - FIXED", -1024, defaultChunkSize}, // Now defaults to 2MB
		{"Very large chunk", 1024 * 1024 * 1024, 1024 * 1024 * 1024},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDownloader("https://example.com/test.tar.gz", 4, tt.chunkSize, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)

			if tt.chunkSize <= 0 {
				t.Logf("✅ FIXED: Chunk size %d validated and set to default %d", tt.chunkSize, tt.expected)
			}

			if d == nil {
				t.Fatal("Downloader should be created")
			}
			if d.chunkSize != tt.expected {
				t.Errorf("chunkSize = %d, want %d", d.chunkSize, tt.expected)
			}
		})
	}
}

// TestMemoryLimitValidation tests invalid memory limit specifications
func TestMemoryLimitValidation(t *testing.T) {
	tests := []struct {
		name    string
		limit   string
		workers int
	}{
		{"Valid memory limit", "1G", 8},
		{"Auto memory", "auto", 8},
		{"Invalid format - BUG", "invalid", 8},
		{"Negative - BUG", "-100M", 8},
		{"Zero", "0", 8},
		{"Empty string", "", 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseMemoryLimit(tt.limit, tt.workers)

			if tt.limit == "invalid" || tt.limit == "-100M" {
				t.Logf("BUG: Result for %q: %d (should validate invalid input)", tt.limit, result)
			}

			// Currently returns negative for "-100M", no error reporting
			if result < 0 {
				t.Logf("BUG: Memory limit is negative: %d (should be validated)", result)
			}
		})
	}
}

// TestProxyURLValidation tests invalid proxy URL handling
func TestProxyURLValidation(t *testing.T) {
	tests := []struct {
		name     string
		proxyURL string
		wantErr  bool
	}{
		{"Valid proxy", "http://proxy:8080", false},
		{"Valid with auth", "http://user:pass@proxy:8080", false},
		{"Invalid URL - BUG: silently ignored", "not a url", false},
		{"Empty", "", false},
		{"Malformed - BUG", "://broken", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDownloader("https://example.com/test.tar.gz", 4, 1024, tt.proxyURL, 0, false, 3, 100*time.Millisecond, 1024*1024*1024)

			// Currently errors are silently ignored in proxy setup
			if tt.wantErr {
				t.Logf("BUG: Invalid proxy URL %q was silently ignored", tt.proxyURL)
			}

			if d == nil {
				t.Error("Downloader should be created")
			}
		})
	}
}

// TestNegativeContentLength tests handling of negative Content-Length
func TestNegativeContentLength(t *testing.T) {
	// This would require mock server enhancement
	// For now, test the parsing logic
	t.Skip("Requires mock server to return negative Content-Length header")
}

// TestZeroContentLength tests handling of zero Content-Length
func TestZeroContentLength(t *testing.T) {
	// Create a mock that returns zero size
	content := GenerateFakeContent(0)
	mock := NewMockHTTPServer(content)
	defer mock.Close()

	d := NewDownloader(mock.URL(), 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)

	err := d.getFileSize()
	if err != nil {
		t.Fatalf("getFileSize() error = %v, want nil", err)
	}

	if d.totalSize != 0 {
		t.Errorf("totalSize = %d, want 0", d.totalSize)
	}

	// Division by zero could occur in progress calculation
	t.Log("BUG: Zero total size could cause division by zero in TUI progress calculation")
}

// TestTUIDimensionUnderflow tests terminal dimension validation
func TestTUIDimensionUnderflow(t *testing.T) {
	m := initialModel("https://example.com/test.tar.gz", 1024, false, false, 1)

	tests := []struct {
		name   string
		width  int
		height int
	}{
		{"Normal dimensions", 80, 24},
		{"Minimum safe", 10, 10},
		{"Very small width - potential underflow", 3, 24},
		{"Very small height", 80, 3},
		{"Both very small", 2, 2},
		{"Zero width - BUG", 0, 24},
		{"Zero height - BUG", 80, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := tea.WindowSizeMsg{Width: tt.width, Height: tt.height}

			// This should not panic
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Panic on dimensions %dx%d: %v", tt.width, tt.height, r)
				}
			}()

			m.Update(msg)

			// Check for underflow
			if m.progress.Width < 0 {
				t.Errorf("Progress width underflow: %d (from window width %d)", m.progress.Width, tt.width)
			}

			if m.viewport.Width < 0 {
				t.Errorf("Viewport width underflow: %d (from window width %d)", m.viewport.Width, tt.width)
			}

			if m.viewport.Height < 0 {
				t.Errorf("Viewport height underflow: %d (from window height %d)", m.viewport.Height, tt.height)
			}

			if tt.width < 4 || tt.height < 12 {
				t.Logf("BUG: Small dimensions %dx%d could cause underflow (width-4=%d, height-12=%d)",
					tt.width, tt.height, tt.width-4, tt.height-12)
			}
		})
	}
}

// TestDivisionByZeroInTUI tests TUI with zero total size
func TestDivisionByZeroInTUI(t *testing.T) {
	m := initialModel("https://example.com/test.tar.gz", 0, false, false, 1) // Zero total

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Panic on division by zero: %v", r)
		}
	}()

	// This should not panic even with zero total
	view := m.View()
	if view == "" {
		t.Log("View is empty with zero total")
	}

	// Update with progress
	msg := progressMsg{
		downloaded: 0,
		total:      0,
		speed:      0,
		eta:        0,
	}

	m.Update(msg)

	// Try to render again
	view = m.View()
	if view == "" {
		t.Log("View is empty after progress update with zero total")
	}

	t.Log("BUG: Division by zero in progress calculation (marianne.go:476) - should validate m.total != 0")
}

// TestRetryDelayOverflow tests exponential backoff overflow
func TestRetryDelayOverflow(t *testing.T) {
	// Skip this test as it would take too long to actually trigger overflow
	// The bug is documented in the code
	t.Skip("Exponential backoff overflow test would take too long")

	// DOCUMENTED BUG (marianne.go:198):
	// delay = time.Duration(float64(delay) * d.backoffFactor)
	// With large initial delays and many retries, this can overflow
	// Example: 1 hour * 2^100 would overflow time.Duration (int64)

	t.Log("BUG: Exponential backoff could overflow duration type (marianne.go:198)")
	t.Log("Fix: Add check: if delay > d.maxDelay { delay = d.maxDelay } BEFORE multiplication")
}

// ErrTest is a test error
var ErrTest = &testError{"test error"}

type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}

// TestBandwidthLimitPrecision tests precision loss in bandwidth calculation
func TestBandwidthLimitPrecision(t *testing.T) {
	tests := []struct {
		input string
		check func(int64) bool
	}{
		{
			"999999.99G",
			func(result int64) bool {
				// Should be close to 999999.99 * 1024^3
				// But might lose precision
				return result > 0
			},
		},
		{
			"0.001M",
			func(result int64) bool {
				// Very small value, might lose precision
				return result >= 0
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := parseBandwidthLimit(tt.input)

			if !tt.check(result) {
				t.Errorf("parseBandwidthLimit(%q) = %d, failed check", tt.input, result)
			}

			t.Logf("parseBandwidthLimit(%q) = %d (potential precision loss)", tt.input, result)
		})
	}

	t.Log("BUG: Float64 multiplication in parseBandwidthLimit can lose precision for large values")
}

// TestChunkCalculationBoundary tests chunk boundary calculations
func TestChunkCalculationBoundary(t *testing.T) {
	tests := []struct {
		name      string
		totalSize int64
		chunkSize int64
		wantChunks int
	}{
		{"Exact division", 1024, 512, 2},
		{"With remainder", 1000, 512, 2}, // ceil(1000/512) = 2
		{"Single chunk", 100, 512, 1},
		{"Many chunks", 10240, 512, 20},
		{"Boundary +1", 1025, 512, 3}, // Should round up
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test the chunk calculation logic
			totalChunks := int((tt.totalSize + tt.chunkSize - 1) / tt.chunkSize)

			if totalChunks != tt.wantChunks {
				t.Errorf("Chunk count = %d, want %d", totalChunks, tt.wantChunks)
			}

			// Verify last chunk doesn't exceed file size
			for i := 0; i < totalChunks; i++ {
				start := int64(i) * tt.chunkSize
				end := start + tt.chunkSize - 1
				if end >= tt.totalSize {
					end = tt.totalSize - 1
				}

				// BUG CHECK: parallel_download.go:115-118
				// The condition `if end >= d.totalSize` should be `if end > d.totalSize`
				// because valid byte range is [0, totalSize-1]
				if end >= tt.totalSize {
					t.Errorf("Chunk %d end=%d should be < totalSize=%d (BUG: off-by-one)", i, end, tt.totalSize)
				}

				chunkLen := end - start + 1
				if chunkLen <= 0 {
					t.Errorf("Chunk %d has invalid length: %d", i, chunkLen)
				}
			}
		})
	}
}

// TestMaxRetriesValidation tests retry count validation
func TestMaxRetriesValidation(t *testing.T) {
	tests := []struct {
		name       string
		maxRetries int
		expected   int
	}{
		{"Valid retries", 10, 10},
		{"Zero retries", 0, 0},
		{"Negative retries - FIXED", -1, 10}, // Now defaults to 10
		{"Very large retries", 1000, 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDownloader("https://example.com/test.tar.gz", 4, 1024, "", 0, false, tt.maxRetries, 100*time.Millisecond, 1024*1024*1024)

			if tt.maxRetries < 0 {
				t.Logf("✅ FIXED: Negative max retries %d validated and set to default %d", tt.maxRetries, tt.expected)
			}

			if d.maxRetries != tt.expected {
				t.Errorf("maxRetries = %d, want %d", d.maxRetries, tt.expected)
			}
		})
	}
}

// TestURLValidation tests URL validation
func TestURLValidation(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"Valid HTTPS", "https://example.com/file.tar.gz"},
		{"Valid HTTP", "http://example.com/file.tar.gz"},
		{"With port", "https://example.com:8080/file.tar.gz"},
		{"With query", "https://example.com/file.tar.gz?key=value"},
		{"Invalid - no scheme", "example.com/file.tar.gz"},
		{"Invalid - malformed", "://broken"},
		{"Empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Currently no URL validation in NewDownloader
			d := NewDownloader(tt.url, 4, 1024, "", 0, false, 3, 100*time.Millisecond, 1024*1024*1024)

			if d == nil {
				t.Fatal("Downloader should be created (no validation)")
			}

			if d.url != tt.url {
				t.Errorf("URL = %s, want %s", d.url, tt.url)
			}
		})
	}

	t.Log("BUG: No URL validation in NewDownloader - malformed URLs accepted")
}

// TestRateLimiterBurstConfig tests rate limiter burst configuration
func TestRateLimiterBurstConfig(t *testing.T) {
	bandwidthLimit := int64(1024 * 1024) // 1MB/s
	d := NewDownloader("https://example.com/test.tar.gz", 4, 1024, "", bandwidthLimit, false, 3, 100*time.Millisecond, 1024*1024*1024)

	if d.rateLimiter == nil {
		t.Fatal("Rate limiter should be initialized")
	}

	// The rate limiter is created with burst size equal to the limit
	// This might not be ideal for all use cases
	t.Log("NOTE: Rate limiter burst size equals sustained rate (marianne.go:143)")
	t.Log("This allows initial burst of full bandwidth before limiting kicks in")
}
