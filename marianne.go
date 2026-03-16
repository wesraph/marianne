package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
	"golang.org/x/time/rate"
)

// Retry configuration
const (
	defaultMaxRetries    = 10               // Maximum number of retry attempts
	defaultInitialDelay  = 1 * time.Second  // Initial retry delay
	defaultMaxDelay      = 30 * time.Second // Maximum retry delay
	defaultBackoffFactor = 2.0              // Exponential backoff multiplier
)

const (
	defaultWorkers       = 8                   // Balanced worker count
	defaultChunkSize     = 2 * 1024 * 1024     // 2MB chunks for better granularity
	chunkReadBufferSize  = 256 * 1024          // 256KB buffer for reading chunks
	maxWorkersLimit      = 1000                // Maximum allowed workers
	validationTimeout    = 30 * time.Second    // Timeout for URL validation HEAD requests
)

var (
	// Version is set at build time
	Version = "dev"

	// readBufPool reuses read buffers across chunk downloads to reduce allocations
	readBufPool = sync.Pool{
		New: func() any {
			buf := make([]byte, chunkReadBufferSize)
			return &buf
		},
	}
)

type Downloader struct {
	url            string
	workers        int
	chunkSize      int64
	client         *http.Client
	totalSize      int64
	downloaded     atomic.Int64 // Use atomic for thread-safe counter
	startTime      time.Time
	rateLimiter    *rate.Limiter
	bandwidthLimit int64
	verbose        bool
	maxRetries     int
	initialDelay   time.Duration
	maxDelay       time.Duration
	backoffFactor  float64
	memoryLimit    int64 // Memory limit for buffering chunks
}

func NewDownloader(url string, workers int, chunkSize int64, proxyURL string, bandwidthLimit int64, verbose bool, maxRetries int, retryDelay time.Duration, memoryLimit int64) (*Downloader, error) {
	// Validate input parameters
	if workers <= 0 {
		workers = 1 // Default to 1 worker
	}
	if workers > maxWorkersLimit {
		workers = maxWorkersLimit // Cap at reasonable maximum
	}
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize // Use default chunk size
	}
	if memoryLimit < 0 {
		memoryLimit = 0 // Will use default in parallel download
	}
	if maxRetries < 0 {
		maxRetries = defaultMaxRetries // Use default retries
	}

	// Size connection pool to match worker count to avoid excessive idle memory
	poolSize := workers * 2
	if poolSize < 16 {
		poolSize = 16
	}

	transport := &http.Transport{
		MaxIdleConns:        poolSize,
		MaxIdleConnsPerHost: poolSize,
		MaxConnsPerHost:     0, // No limit
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // We're downloading binary files
		ForceAttemptHTTP2:   true,
		DisableKeepAlives:   false,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	// Configure proxy if provided
	if proxyURL != "" {
		proxy, err := neturl.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(proxy)
	}

	d := &Downloader{
		url:       url,
		workers:   workers,
		chunkSize: chunkSize,
		client: &http.Client{
			Transport: transport,
			Timeout:   0, // No timeout for downloads
		},
		bandwidthLimit: bandwidthLimit,
		verbose:        verbose,
		maxRetries:     maxRetries,
		initialDelay:   retryDelay,
		maxDelay:       defaultMaxDelay,
		backoffFactor:  defaultBackoffFactor,
		memoryLimit:    memoryLimit,
	}

	// Set up rate limiter if bandwidth limit is specified
	if bandwidthLimit > 0 {
		d.rateLimiter = rate.NewLimiter(rate.Limit(bandwidthLimit), int(bandwidthLimit))
	}

	return d, nil
}

// retryWithBackoff executes a function with exponential backoff retry logic
func (d *Downloader) retryWithBackoff(ctx context.Context, operation string, fn func() error) error {
	var lastErr error
	delay := d.initialDelay

	for attempt := 0; attempt <= d.maxRetries; attempt++ {
		// Try the operation
		err := fn()
		if err == nil {
			return nil // Success
		}

		lastErr = err

		// Check if we've exhausted retries
		if attempt == d.maxRetries {
			break
		}

		// Check if context was cancelled
		if ctx != nil {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s cancelled: %w", operation, ctx.Err())
			default:
			}
		}

		// Log retry attempt if verbose
		if d.verbose {
			fmt.Printf("Retry %d/%d for %s after error: %v. Waiting %v before retry...\n",
				attempt+1, d.maxRetries, operation, err, delay)
		}

		// Wait with context cancellation support
		if ctx != nil {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
				// Timer fired, continue to next retry
				// Note: Stop() returns false here but that's OK
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("%s cancelled during retry: %w", operation, ctx.Err())
			}
			// Ensure timer is stopped to release resources
			timer.Stop()
		} else {
			time.Sleep(delay)
		}

		// Calculate next delay with exponential backoff
		delay = time.Duration(float64(delay) * d.backoffFactor)
		if delay > d.maxDelay {
			delay = d.maxDelay
		}
	}

	return fmt.Errorf("%s failed after %d retries: %w", operation, d.maxRetries, lastErr)
}

func (d *Downloader) getFileSize() error {
	var resp *http.Response
	var size int64

	err := d.retryWithBackoff(context.Background(), "get file size", func() error {
		var err error
		resp, err = d.client.Head(d.url)
		if err != nil {
			return fmt.Errorf("failed to get file size: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("failed to get file size: status %d", resp.StatusCode)
		}

		size, err = strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
		if err != nil {
			return fmt.Errorf("failed to parse content length: %w", err)
		}

		return nil
	})

	if err != nil {
		return err
	}

	d.totalSize = size
	return nil
}

func (d *Downloader) downloadChunk(ctx context.Context, start, end int64, writer io.Writer) error {
	return d.downloadChunkWithProgress(ctx, start, end, writer, nil, 0, 0, time.Time{})
}

func (d *Downloader) downloadChunkWithProgress(ctx context.Context, start, end int64, writer io.Writer, p *tea.Program, chunkIndex int, workerID int, startTime time.Time) error {
	return d.retryWithBackoff(ctx, fmt.Sprintf("download chunk %d-%d", start, end), func() error {
		// CRITICAL: Reset buffer before retry to prevent data corruption
		// Without this, partial data from failed attempts accumulates in the buffer
		if buf, ok := writer.(*bytes.Buffer); ok {
			buf.Reset()
		}

		// Add timeout for individual chunks to prevent hanging
		chunkCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)

		req, err := http.NewRequestWithContext(chunkCtx, "GET", d.url, nil)
		if err != nil {
			cancel()
			return err
		}

		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

		resp, err := d.client.Do(req)
		if err != nil {
			cancel()
			return fmt.Errorf("chunk %d-%d request failed: %w", start, end, err)
		}
		defer func() {
			// Drain body before closing to allow HTTP connection reuse,
			// then cancel the context (order matters: drain must run before cancel)
			io.CopyN(io.Discard, resp.Body, d.chunkSize+1)
			resp.Body.Close()
			cancel()
		}()

		// Servers that don't support range requests will return 200 OK with full content
		// This is incompatible with parallel downloading, so we must reject it
		if resp.StatusCode == http.StatusOK {
			return fmt.Errorf("server does not support range requests (got 200 OK instead of 206 Partial Content) - parallel downloads not possible")
		}

		if resp.StatusCode != http.StatusPartialContent {
			return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}

		// Verify Content-Range header matches what we requested
		// Per RFC 7233, 206 responses MUST include Content-Range header
		contentRange := resp.Header.Get("Content-Range")
		if contentRange == "" {
			return fmt.Errorf("server returned 206 without Content-Range header")
		}
		// Parse and validate Content-Range header
		// Format: "bytes start-end/total" or "bytes start-end/*"
		var rangeStart, rangeEnd int64
		var totalOrStar string
		n, parseErr := fmt.Sscanf(contentRange, "bytes %d-%d/%s", &rangeStart, &rangeEnd, &totalOrStar)
		if parseErr != nil || n != 3 {
			return fmt.Errorf("malformed Content-Range header: %s", contentRange)
		}
		if rangeStart != start || rangeEnd != end {
			return fmt.Errorf("Content-Range mismatch: requested bytes %d-%d but got bytes %d-%d", start, end, rangeStart, rangeEnd)
		}

		bufPtr := readBufPool.Get().(*[]byte)
		buf := *bufPtr
		clear(buf)
		defer readBufPool.Put(bufPtr)
		var totalRead int64
		lastProgressTime := time.Now()

		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				// Apply rate limiting if configured
				if d.rateLimiter != nil {
					d.rateLimiter.WaitN(ctx, n)
				}

				if _, werr := writer.Write(buf[:n]); werr != nil {
					return werr
				}

				totalRead += int64(n)

				// Send progress update every 500ms (always send for speed calculation)
				if p != nil && time.Since(lastProgressTime) > 500*time.Millisecond {
					elapsed := time.Since(startTime).Seconds()
					speed := 0.0
					if elapsed > 0 {
						speed = float64(totalRead) / elapsed
					}

					p.Send(chunkProgressMsg{
						chunkIndex:      chunkIndex,
						start:           start,
						end:             end,
						status:          "progress",
						workerID:        workerID,
						startTime:       startTime,
						speed:           speed,
						bytesDownloaded: totalRead,
					})
					lastProgressTime = time.Now()
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}

		// Verify we got the expected number of bytes
		expectedBytes := end - start + 1
		if totalRead != expectedBytes {
			return fmt.Errorf("incomplete chunk: expected %d bytes, got %d bytes", expectedBytes, totalRead)
		}

		return nil
	})
}

type progressMsg struct {
	downloaded int64
	total      int64
	speed      float64
	eta        time.Duration
}

type fileExtractedMsg string
type downloadCompleteMsg struct{}
type errorMsg error

type partProgressMsg struct {
	partIndex  int
	totalParts int
	url        string
}

type chunkProgressMsg struct {
	chunkIndex      int
	start           int64
	end             int64
	status          string // "started", "completed", "failed", "progress"
	workerID        int
	startTime       time.Time
	speed           float64 // bytes per second
	bytesDownloaded int64   // bytes downloaded so far for this chunk
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

type model struct {
	url            string
	progress       progress.Model
	viewport       viewport.Model
	downloaded     int64
	total          int64
	speed          float64
	avgSpeed       float64
	eta            time.Duration
	startTime      time.Time
	extractedFiles []string
	width          int
	height         int
	done           bool
	err            error
	verbose        bool
	chunkProgress  map[int]chunkProgressMsg

	// Extraction tracking
	extractionCount      int
	extractionSpeed      float64 // files per second
	lastExtCountSnap     int
	lastExtSpeedTime     time.Time
	extractionBytes      int64   // total bytes sent to extractor
	extractionBytesSpeed float64 // bytes per second to extractor
	lastExtBytes         int64
	lastExtBytesTime     time.Time

	// Multi-part fields
	isMultiPart bool
	currentPart int // 1-indexed for display
	totalParts  int
}

func initialModel(url string, totalSize int64, verbose bool, isMultiPart bool, totalParts int) model {
	prog := progress.New(progress.WithDefaultGradient())
	vp := viewport.New(80, 10)
	vp.Style = lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		PaddingLeft(1).
		PaddingRight(1)

	return model{
		url:              url,
		progress:         prog,
		viewport:         vp,
		total:            totalSize,
		startTime:        time.Now(),
		verbose:          verbose,
		chunkProgress:    make(map[int]chunkProgressMsg),
		lastExtSpeedTime: time.Now(),
		lastExtBytesTime: time.Now(),
		isMultiPart:      isMultiPart,
		totalParts:       totalParts,
		currentPart:      1,
	}
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		// Prevent negative width (minimum 10 for reasonable display)
		progressWidth := msg.Width - 4
		if progressWidth < 10 {
			progressWidth = 10
		}
		m.progress.Width = progressWidth
		m.viewport.Width = progressWidth

		// Calculate viewport height with fixed space for chunks to prevent UI shifting
		// Base UI: header(3) + progress bar(2) + stats(2) + files header(2) + padding(4) = 13 lines
		// Chunk details: header(2) + up to 10 chunk lines = 12 lines max (always reserved)
		const maxChunkLines = 12 // 2 (header) + 10 (max chunk display)

		// Always reserve space for chunks to prevent UI from shifting when they appear/disappear
		reservedLines := 13 + maxChunkLines

		// Ensure minimum viewport height of 5 lines
		viewportHeight := msg.Height - reservedLines
		if viewportHeight < 5 {
			viewportHeight = 5
		}
		m.viewport.Height = viewportHeight

	case progressMsg:
		m.downloaded = msg.downloaded
		m.eta = msg.eta

		// Calculate speed from sum of all active chunk speeds (more accurate for parallel downloads)
		totalChunkSpeed := 0.0
		for _, chunk := range m.chunkProgress {
			if chunk.status == "progress" || chunk.status == "started" {
				totalChunkSpeed += chunk.speed
			}
		}
		// Use chunk-based speed if available, otherwise fallback to message speed
		if totalChunkSpeed > 0 {
			m.speed = totalChunkSpeed
		} else {
			m.speed = msg.speed
		}

		if m.total > 0 {
			elapsed := time.Since(m.startTime).Seconds()
			if elapsed > 0 {
				m.avgSpeed = float64(m.downloaded) / elapsed
			}
		}

	case fileExtractedMsg:
		m.extractedFiles = append(m.extractedFiles, string(msg))
		// Keep only last 1000 entries to prevent unbounded memory growth
		// CRITICAL: Create new slice to release backing array memory
		if len(m.extractedFiles) > 1000 {
			newSlice := make([]string, 1000)
			copy(newSlice, m.extractedFiles[len(m.extractedFiles)-1000:])
			m.extractedFiles = newSlice
		}
		content := strings.Join(m.extractedFiles, "\n")
		m.viewport.SetContent(content)
		m.viewport.GotoBottom()

		// Track extraction speed
		m.extractionCount++
		if elapsed := time.Since(m.lastExtSpeedTime).Seconds(); elapsed > 0.5 {
			delta := m.extractionCount - m.lastExtCountSnap
			m.extractionSpeed = float64(delta) / elapsed
			m.lastExtCountSnap = m.extractionCount
			m.lastExtSpeedTime = time.Now()
		}

	case partProgressMsg:
		m.currentPart = msg.partIndex + 1 // 1-indexed for display
		m.url = msg.url

	case chunkProgressMsg:
		// Update chunk progress map (for speed calculation and display)
		m.chunkProgress[msg.chunkIndex] = msg

		// Track extraction throughput when a chunk finishes being sent to extractor
		if msg.status == "extracted" {
			chunkSize := msg.end - msg.start + 1
			m.extractionBytes += chunkSize
			if elapsed := time.Since(m.lastExtBytesTime).Seconds(); elapsed > 0.5 {
				bytesDelta := m.extractionBytes - m.lastExtBytes
				m.extractionBytesSpeed = float64(bytesDelta) / elapsed
				m.lastExtBytes = m.extractionBytes
				m.lastExtBytesTime = time.Now()
			}
		}

		if m.verbose {
			// Only add status changes to extracted files (not progress updates)
			if msg.status != "progress" {
				chunkInfo := fmt.Sprintf("Chunk %d [%s-%s] Worker %d: %s",
					msg.chunkIndex,
					formatBytes(msg.start),
					formatBytes(msg.end),
					msg.workerID,
					msg.status)
				m.extractedFiles = append(m.extractedFiles, chunkInfo)

				// Keep only last 100 entries in verbose mode to prevent memory issues
				// CRITICAL: Create new slice to release backing array memory
				if len(m.extractedFiles) > 100 {
					newSlice := make([]string, 100)
					copy(newSlice, m.extractedFiles[len(m.extractedFiles)-100:])
					m.extractedFiles = newSlice
				}

				content := strings.Join(m.extractedFiles, "\n")
				m.viewport.SetContent(content)
				m.viewport.GotoBottom()
			}
		}

		// Clean up finished chunks from the map to prevent unbounded growth
		// This runs in the TUI update loop so it's thread-safe
		if msg.status == "completed" || msg.status == "failed" || msg.status == "extracted" {
			if len(m.chunkProgress) > 100 {
				for idx, chunk := range m.chunkProgress {
					if (chunk.status == "completed" || chunk.status == "failed" || chunk.status == "extracted") && idx != msg.chunkIndex {
						delete(m.chunkProgress, idx)
						if len(m.chunkProgress) <= 100 {
							break
						}
					}
				}
			}
		}

	case downloadCompleteMsg:
		m.done = true
		return m, tea.Quit

	case errorMsg:
		m.err = msg
		return m, tea.Quit
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("\n❌ Error: %v\n", m.err)
	}

	if m.done {
		totalTime := time.Since(m.startTime)
		return fmt.Sprintf("\n✅ Download completed!\nTotal: %s | Time: %s | Avg Speed: %s/s\n",
			formatBytes(m.total),
			formatDuration(totalTime),
			formatBytes(int64(m.avgSpeed)))
	}

	// Header with URL
	var header string
	if m.isMultiPart {
		header = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("211")).
			MarginBottom(1).
			Render(fmt.Sprintf("📥 Downloading Multi-Part Archive (Part %d/%d)\n%s",
				m.currentPart, m.totalParts, m.url))
	} else {
		header = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("211")).
			MarginBottom(1).
			Render(fmt.Sprintf("📥 Downloading: %s", m.url))
	}

	// Progress bar (avoid division by zero)
	var prog string
	if m.total > 0 {
		prog = m.progress.ViewAs(float64(m.downloaded) / float64(m.total))
	} else {
		prog = m.progress.ViewAs(0)
	}

	// Stats (avoid division by zero)
	var progressPercent float64
	if m.total > 0 {
		progressPercent = float64(m.downloaded) / float64(m.total) * 100
	}

	statsText := fmt.Sprintf(
		"Progress: %.1f%% | Downloaded: %s/%s | Speed: %s/s | Avg: %s/s | ETA: %s",
		progressPercent,
		formatBytes(m.downloaded),
		formatBytes(m.total),
		formatBytes(int64(m.speed)),
		formatBytes(int64(m.avgSpeed)),
		formatDuration(m.eta),
	)
	if m.extractionBytesSpeed > 0 || m.extractionSpeed > 0 {
		statsText += " | Extraction:"
		if m.extractionBytesSpeed > 0 {
			statsText += fmt.Sprintf(" %s/s", formatBytes(int64(m.extractionBytesSpeed)))
		}
		if m.extractionSpeed > 0 {
			statsText += fmt.Sprintf(" (%.0f files/s)", m.extractionSpeed)
		}
	}

	stats := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		MarginTop(1).
		Render(statsText)

	// Build detailed chunk progress view (always show with fixed height to prevent UI shifting)
	const displayLimit = 10
	chunkLines := make([]string, 0, displayLimit)
	activeChunks := 0
	extractingChunks := 0
	completedChunks := 0
	failedChunks := 0

	if len(m.chunkProgress) > 0 {
		// Sort chunks by index for consistent display
		indices := make([]int, 0, len(m.chunkProgress))
		for idx := range m.chunkProgress {
			indices = append(indices, idx)
		}
		sort.Ints(indices)

		// Show only last 10 active/extracting chunks to avoid UI overflow
		activeDisplayed := 0

		for _, idx := range indices {
			chunk := m.chunkProgress[idx]

			// Count chunk statuses
			switch chunk.status {
			case "started", "progress":
				activeChunks++
				if activeDisplayed < displayLimit {
					chunkSize := chunk.end - chunk.start + 1
					elapsed := time.Since(chunk.startTime).Seconds()

					var progressInfo string
					if chunk.bytesDownloaded > 0 {
						chunkPercent := float64(chunk.bytesDownloaded) / float64(chunkSize) * 100
						progressInfo = fmt.Sprintf("%.1f%% (%s/%s) @ %s/s",
							chunkPercent,
							formatBytes(chunk.bytesDownloaded),
							formatBytes(chunkSize),
							formatBytes(int64(chunk.speed)))
					} else {
						progressInfo = fmt.Sprintf("Starting... (%.1fs elapsed)", elapsed)
					}

					chunkLines = append(chunkLines, fmt.Sprintf(
						"  #%-4d [%10s - %10s] Worker %d - %s",
						chunk.chunkIndex,
						formatBytes(chunk.start),
						formatBytes(chunk.end),
						chunk.workerID,
						progressInfo,
					))
					activeDisplayed++
				}
			case "extracting":
				extractingChunks++
				if activeDisplayed < displayLimit {
					chunkSize := chunk.end - chunk.start + 1
					chunkLines = append(chunkLines, fmt.Sprintf(
						"  #%-4d [%10s - %10s] Extracting (%s)...",
						chunk.chunkIndex,
						formatBytes(chunk.start),
						formatBytes(chunk.end),
						formatBytes(chunkSize),
					))
					activeDisplayed++
				}
			case "completed", "extracted":
				completedChunks++
			case "failed":
				failedChunks++
			}
		}
	}

	// Pad chunk lines to always have fixed height (10 lines)
	for len(chunkLines) < displayLimit {
		chunkLines = append(chunkLines, "")
	}

	// Build chunk summary header
	chunkSummary := fmt.Sprintf("Chunks: %d active", activeChunks)
	if extractingChunks > 0 {
		chunkSummary += fmt.Sprintf(", %d extracting", extractingChunks)
	}
	chunkSummary += fmt.Sprintf(", %d completed", completedChunks)
	if failedChunks > 0 {
		chunkSummary += fmt.Sprintf(", %d failed", failedChunks)
	}
	if activeChunks+extractingChunks > displayLimit {
		chunkSummary += fmt.Sprintf(" (showing %d/%d)", displayLimit, activeChunks+extractingChunks)
	}

	chunkHeader := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("214")).
		MarginTop(1).
		MarginBottom(1).
		Render(fmt.Sprintf("⚡ %s", chunkSummary))

	// Always render chunk section with fixed height to prevent UI shifting
	chunkDetailsView := lipgloss.JoinVertical(
		lipgloss.Left,
		chunkHeader,
		strings.Join(chunkLines, "\n"),
	)

	// Extracted files section
	filesHeader := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("212")).
		MarginTop(2).
		MarginBottom(1).
		Render("📂 Extracted Files:")

	// Combine all elements (always include chunk section for fixed layout)
	elements := []string{header, prog, stats, chunkDetailsView, filesHeader, m.viewport.View()}

	return lipgloss.JoinVertical(lipgloss.Left, elements...)
}

type archiveType struct {
	extensions []string
	tarFlag    string
	command    string
}

var archiveTypes = []archiveType{
	{[]string{".tar.gz", ".tgz"}, "-z", ""},
	{[]string{".tar.bz2", ".tbz2", ".tbz"}, "-j", ""},
	{[]string{".tar.xz", ".txz"}, "-J", ""},
	{[]string{".tar.lz4"}, "-I", "lz4"},
	{[]string{".tar.zst", ".tar.zstd"}, "-I", "zstd"},
	{[]string{".tar.lzma"}, "--lzma", ""},
	{[]string{".tar.z"}, "-Z", ""},
	{[]string{".tar"}, "", ""},
}

// buildTarArgs constructs the argument list for the tar command.
// When extractorArgs is provided and tarFlag is "-I", the extractor command and extra args
// are combined into a single -I argument (e.g., -I "zstd -d -T0 --long=31").
func buildTarArgs(tarFlag, tarCommand, extractorArgs, outputDir string) []string {
	args := []string{}
	if tarFlag != "" {
		args = append(args, tarFlag)
		if tarCommand != "" {
			if extractorArgs != "" {
				args = append(args, tarCommand+" "+extractorArgs)
			} else {
				args = append(args, tarCommand)
			}
		}
	}
	args = append(args, "-xvf", "-")
	if outputDir != "" {
		args = append(args, "-C", outputDir)
	}
	return args
}

func detectArchiveType(filename string) (string, string, bool, error) {
	lowerName := strings.ToLower(filename)

	// Check for ZIP first
	if strings.HasSuffix(lowerName, ".zip") {
		return "", "", true, nil
	}

	// Check for tar-based archives
	for _, at := range archiveTypes {
		for _, ext := range at.extensions {
			if strings.HasSuffix(lowerName, ext) {
				return at.tarFlag, at.command, false, nil
			}
		}
	}

	// Check for unsupported archives
	if strings.HasSuffix(lowerName, ".7z") {
		return "", "", false, fmt.Errorf("7z archives are not supported yet")
	}
	if strings.HasSuffix(lowerName, ".rar") {
		return "", "", false, fmt.Errorf("RAR archives are not supported yet")
	}

	return "", "", false, fmt.Errorf("unknown archive type for file: %s", filename)
}

// parseArchiveFormat parses an explicit archive format string and returns the appropriate flags
func parseArchiveFormat(format string) (string, string, bool, error) {
	if format == "" {
		return "", "", false, fmt.Errorf("empty format string")
	}

	format = strings.ToLower(strings.TrimSpace(format))

	// Map of format names to their settings
	formatMap := map[string]struct {
		tarFlag    string
		tarCommand string
		isZip      bool
	}{
		"zip":       {"", "", true},
		"tar":       {"", "", false},
		"tar.gz":    {"-z", "", false},
		"tgz":       {"-z", "", false},
		"tar.bz2":   {"-j", "", false},
		"tbz2":      {"-j", "", false},
		"tbz":       {"-j", "", false},
		"tar.xz":    {"-J", "", false},
		"txz":       {"-J", "", false},
		"tar.lz4":   {"-I", "lz4", false},
		"tar.zst":   {"-I", "zstd", false},
		"tar.zstd":  {"-I", "zstd", false},
		"tar.lzma":  {"--lzma", "", false},
		"tar.z":     {"-Z", "", false},
	}

	settings, ok := formatMap[format]
	if !ok {
		return "", "", false, fmt.Errorf("unsupported archive format: %s (supported: zip, tar, tar.gz, tar.bz2, tar.xz, tar.lz4, tar.zst, tar.lzma, tar.Z)", format)
	}

	return settings.tarFlag, settings.tarCommand, settings.isZip, nil
}

// detectOrUseArchiveType detects archive type from filename or uses forced format if provided
func detectOrUseArchiveType(filename string, forcedFormat string) (string, string, bool, error) {
	if forcedFormat != "" {
		return parseArchiveFormat(forcedFormat)
	}
	return detectArchiveType(filename)
}

func (d *Downloader) Download(ctx context.Context, p *tea.Program, outputDir string, forcedFormat string, extractorArgs string, tracker *cleanupTracker) error {
	// Get file info (skip if already fetched)
	if d.totalSize < 0 {
		if err := d.getFileSize(); err != nil {
			return err
		}
	}

	d.startTime = time.Now()

	// Detect archive type from URL or use forced format
	tarFlag, tarCommand, isZip, err := detectOrUseArchiveType(d.url, forcedFormat)
	if err != nil {
		return err
	}

	// Create output directory if it doesn't exist
	if outputDir != "" {
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	// Check disk space before starting download
	if err := checkDiskSpace(outputDir, d.totalSize); err != nil {
		return err
	}

	if isZip {
		// Handle ZIP files
		return d.downloadAndExtractZip(ctx, p, outputDir, tracker)
	}

	// Handle tar-based archives
	// Build tar command
	args := buildTarArgs(tarFlag, tarCommand, extractorArgs, outputDir)

	cmd := exec.CommandContext(ctx, "tar", args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	// Capture tar output
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start tar command: %w", err)
	}

	// Read tar output - goroutine exits when tar command closes pipes
	go func() {
		scanner := bufio.NewScanner(io.MultiReader(stdout, stderr))
		// Increase buffer size to handle very long filenames (up to 1MB)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
				line := scanner.Text()
				if line != "" {
					p.Send(fileExtractedMsg(line))
				}
			}
		}
	}()

	// Progress reporter
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		lastDownloaded := int64(0)
		lastTime := time.Now()

		for {
			select {
			case <-ticker.C:
				currentDownloaded := d.downloaded.Load()
				currentTime := time.Now()

				// Calculate instantaneous speed for display
				timeDiff := currentTime.Sub(lastTime).Seconds()
				if timeDiff > 0 {
					bytesDiff := currentDownloaded - lastDownloaded
					speed := float64(bytesDiff) / timeDiff

					// Calculate ETA based on average speed since start (more stable)
					totalElapsed := time.Since(d.startTime).Seconds()
					remaining := d.totalSize - currentDownloaded
					var eta time.Duration
					if totalElapsed > 0 && currentDownloaded > 0 {
						avgSpeed := float64(currentDownloaded) / totalElapsed
						eta = time.Duration(float64(remaining)/avgSpeed) * time.Second
					}

					p.Send(progressMsg{
						downloaded: currentDownloaded,
						total:      d.totalSize,
						speed:      speed,
						eta:        eta,
					})

					lastDownloaded = currentDownloaded
					lastTime = currentTime
				}
			case <-done:
				return
			}
		}
	}()

	// Sequential download with parallel chunks
	pipeReader, pipeWriter := io.Pipe()

	// Start copying from pipe to tar stdin
	copyDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdin, pipeReader)
		stdin.Close()
		copyDone <- err
	}()

	// Download chunks - byte counting happens inside downloadInOrderParallel
	downloadErr := d.downloadInOrderParallel(ctx, pipeWriter, p)

	// Close pipe with error if download failed, otherwise close normally
	if downloadErr != nil {
		pipeWriter.CloseWithError(downloadErr)
	} else {
		pipeWriter.Close()
	}

	// Wait for copy to complete
	copyErr := <-copyDone

	close(done)

	if downloadErr != nil {
		cmd.Process.Kill()
		cmd.Wait() // Prevent zombie process
		return downloadErr
	}

	if copyErr != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("copy error: %w", copyErr)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("tar command failed: %w", err)
	}

	p.Send(downloadCompleteMsg{})
	return nil
}

// multiPartCountingWriter tracks bytes across multiple parts
type multiPartCountingWriter struct {
	writer          io.Writer
	totalDownloaded *atomic.Int64
}

func (w *multiPartCountingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		w.totalDownloaded.Add(int64(n))
	}
	return n, err
}

// downloadMultiPart downloads and extracts a multi-part archive
func downloadMultiPart(ctx context.Context, urls []string, partSizes []int64, p *tea.Program,
	outputDir string, workers int, chunkSize int64, proxyURL string,
	limitBytes int64, verbose bool, maxRetries int, retryDelay time.Duration,
	memBytes int64, forcedFormat string, extractorArgs string, tracker *cleanupTracker) error {

	// Detect archive type from first URL or use forced format
	tarFlag, tarCommand, isZip, err := detectOrUseArchiveType(urls[0], forcedFormat)
	if err != nil {
		return err
	}

	// Create output directory if needed
	if outputDir != "" {
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	// Calculate total size and check disk space
	totalSize := int64(0)
	for _, size := range partSizes {
		totalSize += size
	}
	if err := checkDiskSpace(outputDir, totalSize); err != nil {
		return err
	}

	if isZip {
		return downloadMultiPartZip(ctx, urls, partSizes, p, outputDir, workers, chunkSize,
			proxyURL, limitBytes, verbose, maxRetries, retryDelay, memBytes, tracker)
	}

	return downloadMultiPartTar(ctx, urls, partSizes, p, outputDir, tarFlag, tarCommand, extractorArgs,
		workers, chunkSize, proxyURL, limitBytes, verbose,
		maxRetries, retryDelay, memBytes)
}

// downloadMultiPartTar downloads and extracts a multi-part TAR archive with streaming
func downloadMultiPartTar(ctx context.Context, urls []string, partSizes []int64, p *tea.Program,
	outputDir string, tarFlag, tarCommand string, extractorArgs string,
	workers int, chunkSize int64, proxyURL string, limitBytes int64,
	verbose bool, maxRetries int, retryDelay time.Duration, memBytes int64) error {

	// Build tar command (same as single-file path)
	args := buildTarArgs(tarFlag, tarCommand, extractorArgs, outputDir)

	cmd := exec.CommandContext(ctx, "tar", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start tar command: %w", err)
	}

	// Read tar output - goroutine exits when tar command closes pipes
	go func() {
		scanner := bufio.NewScanner(io.MultiReader(stdout, stderr))
		// Increase buffer size to handle very long filenames (up to 1MB)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
				line := scanner.Text()
				if line != "" {
					p.Send(fileExtractedMsg(line))
				}
			}
		}
	}()

	// Create pipe for concatenated parts
	pipeReader, pipeWriter := io.Pipe()

	// Copy from pipe to tar stdin
	copyDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdin, pipeReader)
		stdin.Close()
		copyDone <- err
	}()

	// Calculate total size for progress
	totalSize := int64(0)
	for _, size := range partSizes {
		totalSize += size
	}

	// Shared download counter across all parts
	var totalDownloaded atomic.Int64

	// Progress reporter for overall progress
	done := make(chan struct{})
	startTime := time.Now()
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		lastDownloaded := int64(0)
		lastTime := time.Now()

		for {
			select {
			case <-ticker.C:
				currentDownloaded := totalDownloaded.Load()
				currentTime := time.Now()

				timeDiff := currentTime.Sub(lastTime).Seconds()
				if timeDiff > 0 {
					bytesDiff := currentDownloaded - lastDownloaded
					speed := float64(bytesDiff) / timeDiff

					totalElapsed := time.Since(startTime).Seconds()
					remaining := totalSize - currentDownloaded
					var eta time.Duration
					if totalElapsed > 0 && currentDownloaded > 0 {
						avgSpeed := float64(currentDownloaded) / totalElapsed
						eta = time.Duration(float64(remaining)/avgSpeed) * time.Second
					}

					p.Send(progressMsg{
						downloaded: currentDownloaded,
						total:      totalSize,
						speed:      speed,
						eta:        eta,
					})

					lastDownloaded = currentDownloaded
					lastTime = currentTime
				}
			case <-done:
				return
			}
		}
	}()

	// Download each part sequentially
	for i, url := range urls {
		// Send part progress message
		p.Send(partProgressMsg{
			partIndex:  i,
			totalParts: len(urls),
			url:        url,
		})

		// Create downloader for this part
		d, err := NewDownloader(url, workers, chunkSize, proxyURL, limitBytes, verbose,
			maxRetries, retryDelay, memBytes)
		if err != nil {
			pipeWriter.CloseWithError(err)
			close(done)
			cmd.Process.Kill()
			cmd.Wait() // Prevent zombie process
			return fmt.Errorf("failed to create downloader: %w", err)
		}
		d.totalSize = partSizes[i]

		// Create counting writer that updates overall progress
		countingWriter := &multiPartCountingWriter{
			writer:          pipeWriter,
			totalDownloaded: &totalDownloaded,
		}

		// Download this part
		if err := d.downloadInOrderParallel(ctx, countingWriter, p); err != nil {
			pipeWriter.CloseWithError(err)
			close(done)
			cmd.Process.Kill()
			cmd.Wait() // Prevent zombie process
			return fmt.Errorf("failed to download part %d/%d: %w", i+1, len(urls), err)
		}
	}

	// All parts downloaded
	pipeWriter.Close()
	close(done)

	// Wait for copy to complete
	copyErr := <-copyDone
	if copyErr != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("copy error: %w", copyErr)
	}

	// Wait for tar to complete
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("tar command failed: %w", err)
	}

	p.Send(downloadCompleteMsg{})
	return nil
}

// downloadMultiPartZip downloads and extracts a multi-part ZIP archive
func downloadMultiPartZip(ctx context.Context, urls []string, partSizes []int64, p *tea.Program,
	outputDir string, workers int, chunkSize int64, proxyURL string,
	limitBytes int64, verbose bool, maxRetries int, retryDelay time.Duration,
	memBytes int64, tracker *cleanupTracker) error {

	// Create temp file for combined ZIP
	tmpFile, err := os.CreateTemp("", "marianne-multipart-*.zip")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tempFileName := tmpFile.Name()
	// Register temp file for cleanup on interruption
	if tracker != nil {
		tracker.Add(tempFileName)
	}
	cleanupFile := false // Only cleanup on success
	defer func() {
		tmpFile.Close()
		if cleanupFile {
			// Unregister before removing
			if tracker != nil {
				tracker.Remove(tempFileName)
			}
			os.Remove(tempFileName)
		}
	}()

	// Calculate total size
	totalSize := int64(0)
	for _, size := range partSizes {
		totalSize += size
	}

	var totalDownloaded atomic.Int64

	// Progress reporter
	done := make(chan struct{})
	startTime := time.Now()
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		lastDownloaded := int64(0)
		lastTime := time.Now()

		for {
			select {
			case <-ticker.C:
				currentDownloaded := totalDownloaded.Load()
				currentTime := time.Now()

				timeDiff := currentTime.Sub(lastTime).Seconds()
				if timeDiff > 0 {
					bytesDiff := currentDownloaded - lastDownloaded
					speed := float64(bytesDiff) / timeDiff

					totalElapsed := time.Since(startTime).Seconds()
					remaining := totalSize - currentDownloaded
					var eta time.Duration
					if totalElapsed > 0 && currentDownloaded > 0 {
						avgSpeed := float64(currentDownloaded) / totalElapsed
						eta = time.Duration(float64(remaining)/avgSpeed) * time.Second
					}

					p.Send(progressMsg{
						downloaded: currentDownloaded,
						total:      totalSize,
						speed:      speed,
						eta:        eta,
					})

					lastDownloaded = currentDownloaded
					lastTime = currentTime
				}
			case <-done:
				return
			}
		}
	}()

	// Download each part and append to temp file
	for i, url := range urls {
		p.Send(partProgressMsg{
			partIndex:  i,
			totalParts: len(urls),
			url:        url,
		})

		d, err := NewDownloader(url, workers, chunkSize, proxyURL, limitBytes, verbose,
			maxRetries, retryDelay, memBytes)
		if err != nil {
			close(done)
			return fmt.Errorf("failed to create downloader: %w", err)
		}
		d.totalSize = partSizes[i]

		// Create counting writer
		countingWriter := &multiPartCountingWriter{
			writer:          tmpFile,
			totalDownloaded: &totalDownloaded,
		}

		// Download this part directly to temp file
		if err := d.downloadInOrderParallel(ctx, countingWriter, p); err != nil {
			close(done)
			return fmt.Errorf("failed to download part %d/%d: %w", i+1, len(urls), err)
		}
	}

	close(done)
	tmpFile.Close()

	// Extract the combined ZIP using existing extractZipFile
	dummyDownloader := &Downloader{}
	err = dummyDownloader.extractZipFile(tempFileName, outputDir, p)
	if err == nil {
		cleanupFile = true // Only cleanup on successful extraction
	}
	return err
}

func parseBandwidthLimit(limit string) int64 {
	limit = strings.TrimSpace(strings.ToUpper(limit))
	if limit == "" {
		return 0
	}

	multiplier := int64(1)
	if strings.HasSuffix(limit, "K") {
		multiplier = 1024
		limit = limit[:len(limit)-1]
	} else if strings.HasSuffix(limit, "M") {
		multiplier = 1024 * 1024
		limit = limit[:len(limit)-1]
	} else if strings.HasSuffix(limit, "G") {
		multiplier = 1024 * 1024 * 1024
		limit = limit[:len(limit)-1]
	}

	value, err := strconv.ParseFloat(limit, 64)
	if err != nil {
		return 0
	}

	result := int64(value * float64(multiplier))
	// Return 0 for negative values (invalid)
	if result < 0 {
		return 0
	}
	return result
}

func getSystemMemory() int64 {
	// Try to read from /proc/meminfo on Linux
	data, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "MemTotal:") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					memKB, err := strconv.ParseInt(parts[1], 10, 64)
					if err == nil {
						return memKB * 1024 // Convert KB to bytes
					}
				}
			}
		}
	}

	// Default to 8GB if we can't determine
	return 8 * 1024 * 1024 * 1024
}

// checkDiskSpace verifies there's enough disk space for extraction
// NOTE: This only checks for the compressed archive size + 10% buffer.
// Archives typically decompress to much larger sizes, so this is a conservative
// minimum check. For accurate estimation, we would need to scan archive headers
// which is not practical for streaming extraction.
// Requires at least fileSize + 10% buffer for archive metadata/overhead
func checkDiskSpace(outputDir string, fileSize int64) error {
	if outputDir == "" {
		outputDir = "."
	}

	// Get available disk space
	available, err := getAvailableDiskSpace(outputDir)
	if err != nil {
		// If we can't determine space, log warning but don't fail
		// (might be on unsupported filesystem or platform)
		return nil
	}

	// Require file size + 10% buffer for archive metadata/overhead
	required := int64(float64(fileSize) * 1.1)

	if available < required {
		return fmt.Errorf("insufficient disk space: need %s, but only %s available in %s",
			formatBytes(required), formatBytes(available), outputDir)
	}

	return nil
}

func parseMemoryLimit(limit string, workers int) int64 {
	if limit == "auto" {
		// Use 10% of system memory
		systemMem := getSystemMemory()
		return systemMem / 10
	}

	// Parse the provided limit
	return parseBandwidthLimit(limit)
}

// cleanupTracker tracks temporary files for cleanup on interruption
type cleanupTracker struct {
	mu    sync.Mutex
	files []string
}

func (c *cleanupTracker) Add(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files = append(c.files, path)
}

func (c *cleanupTracker) Remove(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, f := range c.files {
		if f == path {
			c.files = append(c.files[:i], c.files[i+1:]...)
			break
		}
	}
}

func (c *cleanupTracker) Cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.files {
		os.Remove(f)
	}
}

func (d *Downloader) downloadAndExtractZip(ctx context.Context, p *tea.Program, outputDir string, tracker *cleanupTracker) error {
	// Create temp file
	tmpFile, err := os.CreateTemp("", "marianne-*.zip")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tempFileName := tmpFile.Name()
	// Register temp file for cleanup on interruption
	if tracker != nil {
		tracker.Add(tempFileName)
	}
	cleanupFile := false // Only cleanup on success

	defer func() {
		tmpFile.Close()
		if cleanupFile {
			// Unregister before removing
			if tracker != nil {
				tracker.Remove(tempFileName)
			}
			os.Remove(tempFileName)
		}
	}()

	// Progress reporter
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		lastDownloaded := int64(0)
		lastTime := time.Now()

		for {
			select {
			case <-ticker.C:
				currentDownloaded := d.downloaded.Load()
				currentTime := time.Now()

				// Calculate instantaneous speed for display
				timeDiff := currentTime.Sub(lastTime).Seconds()
				if timeDiff > 0 {
					bytesDiff := currentDownloaded - lastDownloaded
					speed := float64(bytesDiff) / timeDiff

					// Calculate ETA based on average speed since start (more stable)
					totalElapsed := time.Since(d.startTime).Seconds()
					remaining := d.totalSize - currentDownloaded
					var eta time.Duration
					if totalElapsed > 0 && currentDownloaded > 0 {
						avgSpeed := float64(currentDownloaded) / totalElapsed
						eta = time.Duration(float64(remaining)/avgSpeed) * time.Second
					}

					p.Send(progressMsg{
						downloaded: currentDownloaded,
						total:      d.totalSize,
						speed:      speed,
						eta:        eta,
					})

					lastDownloaded = currentDownloaded
					lastTime = currentTime
				}
			case <-done:
				return
			}
		}
	}()

	// Download to temp file
	if err := d.downloadToFile(ctx, tmpFile); err != nil {
		close(done)
		return err
	}
	close(done)

	// Extract ZIP file
	err = d.extractZipFile(tempFileName, outputDir, p)
	if err == nil {
		cleanupFile = true // Only cleanup on successful extraction
	}
	return err
}

// validateZipPath checks if a zip entry path is safe (no path traversal)
func validateZipPath(path string) error {
	// Check for null bytes
	if strings.Contains(path, "\x00") {
		return fmt.Errorf("path contains null byte: %s", path)
	}

	// Clean the path and check if absolute
	cleaned := filepath.Clean(path)
	if filepath.IsAbs(cleaned) {
		return fmt.Errorf("path is absolute: %s", path)
	}

	// Check each component for ".."
	parts := strings.Split(cleaned, string(filepath.Separator))
	for _, part := range parts {
		if part == ".." {
			return fmt.Errorf("path traversal attempt: %s", path)
		}
	}

	return nil
}

func (d *Downloader) extractZipFile(filename string, outputDir string, p *tea.Program) error {
	reader, err := zip.OpenReader(filename)
	if err != nil {
		return fmt.Errorf("failed to open zip file: %w", err)
	}
	defer reader.Close()

	// Resolve output directory once outside the loop
	absOutputDir, err := filepath.Abs(outputDir)
	if err != nil {
		return fmt.Errorf("failed to resolve output dir: %w", err)
	}

	// Extract files
	for _, file := range reader.File {
		// Validate file path to prevent path traversal attacks
		if err := validateZipPath(file.Name); err != nil {
			return fmt.Errorf("invalid file path in zip: %w", err)
		}

		path := filepath.Join(outputDir, file.Name)

		// Double-check that the resolved path is within outputDir using filepath.Rel for cross-platform support
		absPath, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("failed to resolve path: %w", err)
		}
		// Use filepath.Rel to check if path is within output directory
		relPath, err := filepath.Rel(absOutputDir, absPath)
		if err != nil || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) || relPath == ".." {
			return fmt.Errorf("invalid file path (path traversal attempt): %s", file.Name)
		}

		// Check for symlinks - validate target to prevent path traversal attacks
		// NOTE: This validation cannot prevent symlink chain attacks where multiple
		// symlinks in the archive combine to escape the output directory. Each symlink
		// is validated independently against the output directory, but chains of symlinks
		// could still potentially escape if carefully crafted.
		mode := file.Mode()
		if mode&os.ModeSymlink != 0 {
			// Read symlink target
			fileReader, err := file.Open()
			if err != nil {
				return fmt.Errorf("failed to read symlink %s: %w", file.Name, err)
			}
			linkTarget, err := io.ReadAll(io.LimitReader(fileReader, 4096))
			fileReader.Close()
			if err != nil {
				return fmt.Errorf("failed to read symlink target for %s: %w", file.Name, err)
			}

			// Resolve the symlink target relative to the symlink location
			symlinkDir := filepath.Dir(path)
			targetPath := filepath.Join(symlinkDir, string(linkTarget))
			absTargetPath, err := filepath.Abs(targetPath)
			if err != nil {
				return fmt.Errorf("failed to resolve symlink target: %w", err)
			}

			// Verify symlink target is within output directory using filepath.Rel
			relTargetPath, err := filepath.Rel(absOutputDir, absTargetPath)
			if err != nil || strings.HasPrefix(relTargetPath, ".."+string(filepath.Separator)) || relTargetPath == ".." {
				return fmt.Errorf("symlink path traversal attempt: %s -> %s (points outside extraction directory)", file.Name, string(linkTarget))
			}

			// Create the symlink
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return fmt.Errorf("failed to create directory for symlink %s: %w", path, err)
			}
			if err := os.Symlink(string(linkTarget), path); err != nil {
				return fmt.Errorf("failed to create symlink %s: %w", path, err)
			}

			p.Send(fileExtractedMsg(file.Name + " (symlink)"))
			continue
		}

		// Send file extraction message
		p.Send(fileExtractedMsg(file.Name))

		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(path, file.Mode()); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", path, err)
			}
			continue
		}

		// Create directory if needed
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}

		// Extract file (close files immediately to avoid FD leak in large archives)
		fileReader, err := file.Open()
		if err != nil {
			return err
		}

		targetFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
		if err != nil {
			fileReader.Close()
			return err
		}

		_, err = io.Copy(targetFile, fileReader)
		fileReader.Close()
		closeErr := targetFile.Close()

		if err != nil {
			return err
		}
		// Check for write errors that may only surface during Close()
		if closeErr != nil {
			return fmt.Errorf("failed to close file %s: %w", path, closeErr)
		}
	}

	p.Send(downloadCompleteMsg{})
	return nil
}

func (d *Downloader) downloadToFile(ctx context.Context, file *os.File) error {
	return d.retryWithBackoff(ctx, "download file", func() error {
		// Reset file position and truncate on retry to prevent corruption
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("failed to seek file: %w", err)
		}
		if err := file.Truncate(0); err != nil {
			return fmt.Errorf("failed to truncate file: %w", err)
		}
		// Reset download counter so progress reporting stays accurate on retry
		d.downloaded.Store(0)

		req, err := http.NewRequestWithContext(ctx, "GET", d.url, nil)
		if err != nil {
			return err
		}

		resp, err := d.client.Do(req)
		if err != nil {
			return err
		}
		defer func() {
			// Drain at most 1MB to allow connection reuse for small error bodies
			io.CopyN(io.Discard, resp.Body, 1<<20)
			resp.Body.Close()
		}()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}

		// Copy with rate limiting
		var reader io.Reader = resp.Body
		if d.rateLimiter != nil {
			reader = &rateLimitedReader{
				reader:     resp.Body,
				limiter:    d.rateLimiter,
				ctx:        ctx,
				downloader: d,
			}
		}

		_, err = io.Copy(file, reader)
		return err
	})
}

type rateLimitedReader struct {
	reader     io.Reader
	limiter    *rate.Limiter
	ctx        context.Context
	downloader *Downloader
}

func (r *rateLimitedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		if waitErr := r.limiter.WaitN(r.ctx, n); waitErr != nil {
			return n, waitErr
		}
		r.downloader.downloaded.Add(int64(n))
	}
	return n, err
}

// validateURLs performs pre-flight checks on all URLs
// Returns slice of part sizes and error if validation fails
func validateURLs(ctx context.Context, urls []string, client *http.Client) ([]int64, error) {
	partSizes := make([]int64, len(urls))

	for i, url := range urls {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// 1. Check URL format
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return nil, fmt.Errorf("part %d: invalid URL format (must start with http:// or https://): %s", i+1, url)
		}

		// 2. HEAD request to get size and check range support
		reqCtx, cancel := context.WithTimeout(ctx, validationTimeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, url, nil)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("part %d: failed to create request: %w", i+1, err)
		}

		resp, err := client.Do(req)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("part %d: cannot reach URL: %w\nURL: %s", i+1, err, url)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("part %d: server returned status %d for URL: %s", i+1, resp.StatusCode, url)
		}

		// 3. Get Content-Length
		size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
		if err != nil || size <= 0 {
			return nil, fmt.Errorf("part %d: cannot determine file size (Content-Length missing or invalid): %s", i+1, url)
		}
		partSizes[i] = size

		// 4. Check range support (required for parallel downloads)
		acceptRanges := resp.Header.Get("Accept-Ranges")
		if acceptRanges != "bytes" {
			// Note: Some servers don't send Accept-Ranges but still support ranges
			// We'll catch this during actual download if it fails
		}
	}

	return partSizes, nil
}

// printValidationSummary prints a summary of validated URLs
func printValidationSummary(urls []string, partSizes []int64) {
	fmt.Println("Validating URLs...")
	totalSize := int64(0)
	for i, url := range urls {
		fmt.Printf("  Part %d/%d: ✓ %s (%s)\n", i+1, len(urls), url, formatBytes(partSizes[i]))
		totalSize += partSizes[i]
	}
	fmt.Printf("\nTotal download size: %s across %d parts\n\n", formatBytes(totalSize), len(urls))
}

func main() {
	var (
		workers        = flag.Int("workers", defaultWorkers, "Number of parallel workers")
		chunkSize      = flag.Int64("chunk", defaultChunkSize, "Chunk size in bytes")
		outputDir      = flag.String("output", "", "Output directory (creates if doesn't exist)")
		proxyURL       = flag.String("proxy", "", "HTTP proxy URL (e.g., http://proxy:8080)")
		bandwidthLimit = flag.String("limit", "", "Bandwidth limit (e.g., 1M, 500K, 2.5M)")
		showVersion    = flag.Bool("version", false, "Show version and exit")
		verbose        = flag.Bool("verbose", false, "Show detailed progress of individual chunks")
		memoryLimit    = flag.String("memory", "auto", "Memory limit for buffers (e.g., 1G, 500M, auto for 10% of system memory)")
		maxRetries     = flag.Int("max-retries", defaultMaxRetries, "Maximum number of retry attempts for failed connections")
		retryDelay     = flag.Duration("retry-delay", defaultInitialDelay, "Initial retry delay (e.g., 1s, 500ms)")
		archiveFormat  = flag.String("format", "", "Force archive format (zip, tar, tar.gz, tar.bz2, tar.xz, tar.lz4, tar.zst, tar.lzma, tar.Z)")
		extractorArgs  = flag.String("extractor-args", "", "Extra arguments for the extractor command (e.g., \"-d -T0 --long=31\" for zstd)")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("Marianne %s\n", Version)
		fmt.Println("A blazing-fast parallel downloader with automatic archive extraction")
		fmt.Println("https://github.com/wesraph/marianne")
		os.Exit(0)
	}

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: marianne [options] <url> [url2] [url3] ...")
		fmt.Fprintln(os.Stderr, "\nOptions:")
		fmt.Fprintln(os.Stderr, "  -workers N     Number of parallel workers (default: 8)")
		fmt.Fprintln(os.Stderr, "  -chunk N       Chunk size in bytes (default: 2MB)")
		fmt.Fprintln(os.Stderr, "  -output DIR    Output directory (creates if doesn't exist)")
		fmt.Fprintln(os.Stderr, "  -proxy URL     HTTP proxy URL (e.g., http://proxy:8080)")
		fmt.Fprintln(os.Stderr, "  -limit RATE    Bandwidth limit (e.g., 1M, 500K, 2.5M)")
		fmt.Fprintln(os.Stderr, "  -max-retries N Maximum retry attempts for failed connections (default: 10)")
		fmt.Fprintln(os.Stderr, "  -retry-delay D Initial retry delay (default: 1s)")
		fmt.Fprintln(os.Stderr, "  -format FMT    Force archive format (overrides auto-detection)")
		fmt.Fprintln(os.Stderr, "  -extractor-args ARGS  Extra arguments for the extractor (e.g., \"-d -T0 --long=31\")")
		fmt.Fprintln(os.Stderr, "\nSupported archive formats:")
		fmt.Fprintln(os.Stderr, "  .zip, .tar, .tar.gz, .tgz, .tar.bz2, .tbz2, .tar.xz, .txz")
		fmt.Fprintln(os.Stderr, "  .tar.lz4, .tar.zst, .tar.zstd, .tar.lzma, .tar.Z")
		fmt.Fprintln(os.Stderr, "\nFormat flag values:")
		fmt.Fprintln(os.Stderr, "  zip, tar, tar.gz, tgz, tar.bz2, tbz2, tar.xz, txz")
		fmt.Fprintln(os.Stderr, "  tar.lz4, tar.zst, tar.zstd, tar.lzma, tar.z")
		fmt.Fprintln(os.Stderr, "\nMulti-part archives:")
		fmt.Fprintln(os.Stderr, "  Provide multiple URLs in order to download and concatenate parts:")
		fmt.Fprintln(os.Stderr, "  marianne file.tar.gz.part1 file.tar.gz.part2 file.tar.gz.part3")
		fmt.Fprintln(os.Stderr, "  marianne -format tar.gz file.001 file.002 file.003")
		os.Exit(1)
	}

	urls := flag.Args()

	// Parse bandwidth limit
	var limitBytes int64
	if *bandwidthLimit != "" {
		limitBytes = parseBandwidthLimit(*bandwidthLimit)
		if limitBytes <= 0 {
			fmt.Fprintf(os.Stderr, "Invalid bandwidth limit: %s\n", *bandwidthLimit)
			os.Exit(1)
		}
	}

	// Parse memory limit
	memBytes := parseMemoryLimit(*memoryLimit, *workers)
	if memBytes <= 0 {
		fmt.Fprintf(os.Stderr, "Invalid memory limit: %s\n", *memoryLimit)
		os.Exit(1)
	}

	// Calculate optimal chunk size based on memory and workers
	perWorkerMem := memBytes / int64(*workers)
	optimalChunkSize := perWorkerMem / 4 // Allow 4 chunks per worker in memory
	if optimalChunkSize < 1024*1024 {    // Minimum 1MB chunks
		optimalChunkSize = 1024 * 1024
	}

	// Use the calculated chunk size unless user specified one
	if *chunkSize == defaultChunkSize {
		*chunkSize = optimalChunkSize
	}

	if *verbose {
		fmt.Printf("Memory limit: %s (chunk size: %s)\n", formatBytes(memBytes), formatBytes(*chunkSize))
	}

	// Determine if multi-part download
	isMultiPart := len(urls) > 1

	var model model
	var totalSize int64
	var partSizes []int64            // Store part sizes for multi-part downloads
	var singleDownloader *Downloader // Reused for single URL path

	if isMultiPart {
		// Multi-part download path
		// Create HTTP client for validation (small pool, only used for HEAD requests)
		transport := &http.Transport{
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 16,
			MaxConnsPerHost:     0,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
			ForceAttemptHTTP2:   true,
			DisableKeepAlives:   false,
			TLSHandshakeTimeout: 10 * time.Second,
		}
		if *proxyURL != "" {
			proxy, err := neturl.Parse(*proxyURL)
			if err == nil {
				transport.Proxy = http.ProxyURL(proxy)
			}
		}
		client := &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second, // For HEAD requests
		}

		// Validate all URLs (using background context since we're not yet in the cancellable part)
		var err error
		partSizes, err = validateURLs(context.Background(), urls, client)
		if err != nil {
			fmt.Fprintf(os.Stderr, "URL validation failed: %v\n", err)
			os.Exit(1)
		}
		transport.CloseIdleConnections()

		// Print validation summary
		printValidationSummary(urls, partSizes)

		// Calculate total size
		for _, size := range partSizes {
			totalSize += size
		}

		// Create TUI with multi-part info
		model = initialModel(urls[0], totalSize, *verbose, true, len(urls))
	} else {
		// Single URL download path - create downloader once and reuse for download
		url := urls[0]
		var err error
		singleDownloader, err = NewDownloader(url, *workers, *chunkSize, *proxyURL, limitBytes, *verbose, *maxRetries, *retryDelay, memBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		// Get file size first
		if err := singleDownloader.getFileSize(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		// Create TUI
		model = initialModel(url, singleDownloader.totalSize, *verbose, false, 1)
	}

	// Check if we're running in a TTY (interactive terminal)
	// If not (e.g., piped output, background, CI/CD), use headless mode
	var p *tea.Program
	if term.IsTerminal(int(os.Stdout.Fd())) {
		// Interactive mode with full TUI
		p = tea.NewProgram(model)
	} else {
		// Non-interactive mode - disable TUI to avoid /dev/tty errors
		p = tea.NewProgram(model,
			tea.WithInput(nil),
			tea.WithoutRenderer(),
		)
	}

	// Create cleanup tracker for temp files
	tracker := &cleanupTracker{}

	// Set up signal handler for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	// Create context that can be cancelled
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run download in background
	errChan := make(chan error, 1)
	go func() {
		var err error
		if isMultiPart {
			// Multi-part download - use already validated partSizes
			err = downloadMultiPart(ctx, urls, partSizes, p, *outputDir, *workers, *chunkSize,
				*proxyURL, limitBytes, *verbose, *maxRetries, *retryDelay, memBytes, *archiveFormat, *extractorArgs, tracker)
		} else {
			// Single URL download - reuse the downloader created earlier
			err = singleDownloader.Download(ctx, p, *outputDir, *archiveFormat, *extractorArgs, tracker)
		}

		if err != nil {
			p.Send(errorMsg(err))
			errChan <- err
		} else {
			errChan <- nil
		}
	}()

	// Track if signal was received to coordinate error handling
	signalReceived := make(chan struct{})
	tuiDone := make(chan struct{})

	// Handle signals in background
	go func() {
		select {
		case <-sigChan:
			close(signalReceived)
			// Stop listening for more signals immediately
			signal.Stop(sigChan)
			// Cancel context to stop download first
			cancel()
			// Wait for download goroutine to finish (with timeout)
			// This ensures files are properly closed before cleanup
			select {
			case <-errChan:
				// Download finished, safe to cleanup
			case <-time.After(2 * time.Second):
				// Timeout - cleanup anyway to avoid hanging
			}
			// Now cleanup temp files
			tracker.Cleanup()
			// Quit the TUI
			p.Quit()
		case <-tuiDone:
			// Normal exit - stop listening for signals and exit cleanly
			signal.Stop(sigChan)
			return
		}
	}()

	// Run TUI
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running TUI: %v\n", err)
		close(tuiDone) // Signal goroutine to exit
		time.Sleep(10 * time.Millisecond) // Brief wait for goroutine cleanup
		signal.Stop(sigChan) // Backup cleanup
		os.Exit(1)
	}

	// Signal that TUI is done (allows signal handler goroutine to exit)
	close(tuiDone)

	// Brief wait to let signal handler goroutine complete cleanup
	time.Sleep(10 * time.Millisecond)

	// Check for download errors (only if signal wasn't received, as signal handler consumes errChan)
	select {
	case <-signalReceived:
		// Signal was received, error already handled by signal goroutine
	default:
		// No signal, check for download errors
		select {
		case err := <-errChan:
			if err != nil && err != context.Canceled {
				os.Exit(1)
			}
		default:
			// Download completed without error
		}
	}
}
