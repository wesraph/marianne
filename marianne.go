package main

import (
	"archive/zip"
	"bufio"
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
	defaultWorkers   = 8               // Balanced worker count
	defaultChunkSize = 2 * 1024 * 1024 // 2MB chunks for better granularity
)

var (
	// Version is set at build time
	Version = "dev"
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

func NewDownloader(url string, workers int, chunkSize int64, proxyURL string, bandwidthLimit int64, verbose bool, maxRetries int, retryDelay time.Duration, memoryLimit int64) *Downloader {
	// Validate input parameters
	if workers <= 0 {
		workers = 1 // Default to 1 worker
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

	transport := &http.Transport{
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 1000,
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
		if err == nil {
			transport.Proxy = http.ProxyURL(proxy)
		}
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

	return d
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
				// Continue to next retry
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("%s cancelled during retry: %w", operation, ctx.Err())
			}
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
		// Add timeout for individual chunks to prevent hanging
		chunkCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		req, err := http.NewRequestWithContext(chunkCtx, "GET", d.url, nil)
		if err != nil {
			return err
		}

		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

		resp, err := d.client.Do(req)
		if err != nil {
			return fmt.Errorf("chunk %d-%d request failed: %w", start, end, err)
		}
		defer resp.Body.Close()

		// Servers that don't support range requests will return 200 OK with full content
		// This is incompatible with parallel downloading, so we must reject it
		if resp.StatusCode == http.StatusOK {
			return fmt.Errorf("server does not support range requests (got 200 OK instead of 206 Partial Content) - parallel downloads not possible")
		}

		if resp.StatusCode != http.StatusPartialContent {
			return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}

		buf := make([]byte, 256*1024) // 256KB buffer to reduce memory usage
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
		url:           url,
		progress:      prog,
		viewport:      vp,
		total:         totalSize,
		startTime:     time.Now(),
		verbose:       verbose,
		chunkProgress: make(map[int]chunkProgressMsg),
		isMultiPart:   isMultiPart,
		totalParts:    totalParts,
		currentPart:   1,
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
		m.progress.Width = msg.Width - 4
		m.viewport.Width = msg.Width - 4

		// Calculate viewport height with fixed space for chunks to prevent UI shifting
		// Base UI: header(3) + progress bar(2) + stats(2) + files header(2) + padding(4) = 13 lines
		// Chunk details: header(2) + up to 10 chunk lines = 12 lines max (always reserved)
		const maxChunkLines = 12 // 2 (header) + 10 (max chunk display)

		// Always reserve space for chunks to prevent UI from shifting when they appear/disappear
		reservedLines := 13 + maxChunkLines

		// Ensure minimum viewport height of 5 lines
		if msg.Height-reservedLines < 5 {
			m.viewport.Height = 5
		} else {
			m.viewport.Height = msg.Height - reservedLines
		}

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
			m.avgSpeed = float64(m.downloaded) / time.Since(m.startTime).Seconds()
		}

	case fileExtractedMsg:
		m.extractedFiles = append(m.extractedFiles, string(msg))
		content := strings.Join(m.extractedFiles, "\n")
		m.viewport.SetContent(content)
		m.viewport.GotoBottom()

	case partProgressMsg:
		m.currentPart = msg.partIndex + 1 // 1-indexed for display
		m.url = msg.url

	case chunkProgressMsg:
		// Update chunk progress map (for speed calculation and display)
		m.chunkProgress[msg.chunkIndex] = msg

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
				if len(m.extractedFiles) > 100 {
					m.extractedFiles = m.extractedFiles[len(m.extractedFiles)-100:]
				}

				content := strings.Join(m.extractedFiles, "\n")
				m.viewport.SetContent(content)
				m.viewport.GotoBottom()
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

	stats := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		MarginTop(1).
		Render(statsText)

	// Build detailed chunk progress view (always show with fixed height to prevent UI shifting)
	const displayLimit = 10
	chunkLines := make([]string, 0, displayLimit)
	activeChunks := 0
	completedChunks := 0
	failedChunks := 0

	if len(m.chunkProgress) > 0 {
		// Sort chunks by index for consistent display
		indices := make([]int, 0, len(m.chunkProgress))
		for idx := range m.chunkProgress {
			indices = append(indices, idx)
		}
		sort.Ints(indices)

		// Show only last 10 active chunks to avoid UI overflow
		activeDisplayed := 0

		for _, idx := range indices {
			chunk := m.chunkProgress[idx]

			// Count chunk statuses
			switch chunk.status {
			case "started", "progress":
				activeChunks++ // Count both started and in-progress chunks as active
				// Only display active/in-progress chunks up to limit
				if activeDisplayed < displayLimit {
					chunkSize := chunk.end - chunk.start + 1
					elapsed := time.Since(chunk.startTime).Seconds()

					// Display real-time download progress and speed
					var progressInfo string
					if chunk.bytesDownloaded > 0 {
						progressPercent := float64(chunk.bytesDownloaded) / float64(chunkSize) * 100
						progressInfo = fmt.Sprintf("%.1f%% (%s/%s) @ %s/s",
							progressPercent,
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
			case "completed":
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
	chunkSummary := fmt.Sprintf("Chunks: %d active, %d completed", activeChunks, completedChunks)
	if failedChunks > 0 {
		chunkSummary += fmt.Sprintf(", %d failed", failedChunks)
	}
	if activeChunks > displayLimit {
		chunkSummary += fmt.Sprintf(" (showing %d/%d active)", displayLimit, activeChunks)
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
	{[]string{".tar.Z"}, "-Z", ""},
	{[]string{".tar"}, "", ""},
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

func (d *Downloader) Download(ctx context.Context, p *tea.Program, outputDir string, forcedFormat string) error {
	// Get file info
	if err := d.getFileSize(); err != nil {
		return err
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

	if isZip {
		// Handle ZIP files
		return d.downloadAndExtractZip(ctx, p, outputDir)
	}

	// Handle tar-based archives
	// Build tar command
	args := []string{}
	if tarFlag != "" {
		args = append(args, tarFlag)
		if tarCommand != "" {
			args = append(args, tarCommand)
		}
	}

	args = append(args, "-xvf", "-")

	if outputDir != "" {
		args = append(args, "-C", outputDir)
	}

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

	// Read tar output with timeout protection
	scannerDone := make(chan struct{})
	go func() {
		defer close(scannerDone)
		scanner := bufio.NewScanner(io.MultiReader(stdout, stderr))
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

	// Create a counting writer to track bytes written
	countingWriter := &countingWriter{
		writer:     pipeWriter,
		downloader: d,
	}

	// Start copying from pipe to tar stdin
	copyDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdin, pipeReader)
		stdin.Close()
		copyDone <- err
	}()

	// Download chunks
	downloadErr := d.downloadInOrderParallel(ctx, countingWriter, p)
	pipeWriter.Close()

	// Wait for copy to complete
	copyErr := <-copyDone

	close(done)

	if downloadErr != nil {
		cmd.Process.Kill()
		return downloadErr
	}

	if copyErr != nil {
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
	memBytes int64, forcedFormat string) error {

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

	if isZip {
		return downloadMultiPartZip(ctx, urls, partSizes, p, outputDir, workers, chunkSize,
			proxyURL, limitBytes, verbose, maxRetries, retryDelay, memBytes)
	}

	return downloadMultiPartTar(ctx, urls, partSizes, p, outputDir, tarFlag, tarCommand,
		workers, chunkSize, proxyURL, limitBytes, verbose,
		maxRetries, retryDelay, memBytes)
}

// downloadMultiPartTar downloads and extracts a multi-part TAR archive with streaming
func downloadMultiPartTar(ctx context.Context, urls []string, partSizes []int64, p *tea.Program,
	outputDir string, tarFlag, tarCommand string,
	workers int, chunkSize int64, proxyURL string, limitBytes int64,
	verbose bool, maxRetries int, retryDelay time.Duration, memBytes int64) error {

	// Build tar command (same as single-file path)
	args := []string{}
	if tarFlag != "" {
		args = append(args, tarFlag)
		if tarCommand != "" {
			args = append(args, tarCommand)
		}
	}
	args = append(args, "-xvf", "-")
	if outputDir != "" {
		args = append(args, "-C", outputDir)
	}

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

	// Read tar output
	scannerDone := make(chan struct{})
	go func() {
		defer close(scannerDone)
		scanner := bufio.NewScanner(io.MultiReader(stdout, stderr))
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
		d := NewDownloader(url, workers, chunkSize, proxyURL, limitBytes, verbose,
			maxRetries, retryDelay, memBytes)
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
			return fmt.Errorf("failed to download part %d/%d: %w", i+1, len(urls), err)
		}
	}

	// All parts downloaded
	pipeWriter.Close()
	close(done)

	// Wait for copy to complete
	copyErr := <-copyDone
	if copyErr != nil {
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
	memBytes int64) error {

	// Create temp file for combined ZIP
	tmpFile, err := os.CreateTemp("", "marianne-multipart-*.zip")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer func() {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
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

		d := NewDownloader(url, workers, chunkSize, proxyURL, limitBytes, verbose,
			maxRetries, retryDelay, memBytes)
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
	return dummyDownloader.extractZipFile(tmpFile.Name(), outputDir, p)
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

	return int64(value * float64(multiplier))
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

func parseMemoryLimit(limit string, workers int) int64 {
	if limit == "auto" {
		// Use 10% of system memory
		systemMem := getSystemMemory()
		return systemMem / 10
	}

	// Parse the provided limit
	return parseBandwidthLimit(limit)
}

func (d *Downloader) downloadAndExtractZip(ctx context.Context, p *tea.Program, outputDir string) error {
	// Create temp file
	tmpFile, err := os.CreateTemp("", "marianne-*.zip")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}

	defer func() {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
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
	return d.extractZipFile(tmpFile.Name(), outputDir, p)
}

// validateZipPath checks if a zip entry path is safe (no path traversal)
func validateZipPath(path string) error {
	// Check for path traversal attempts
	if strings.Contains(path, "..") {
		return fmt.Errorf("path contains '..': %s", path)
	}

	// Check for absolute paths
	if filepath.IsAbs(path) {
		return fmt.Errorf("path is absolute: %s", path)
	}

	// Check for backslashes (Windows path separators used maliciously on Unix)
	if strings.Contains(path, "\\") {
		return fmt.Errorf("path contains backslash: %s", path)
	}

	return nil
}

func (d *Downloader) extractZipFile(filename string, outputDir string, p *tea.Program) error {
	reader, err := zip.OpenReader(filename)
	if err != nil {
		return fmt.Errorf("failed to open zip file: %w", err)
	}
	defer reader.Close()

	// Extract files
	for _, file := range reader.File {
		// Validate file path to prevent path traversal attacks
		if err := validateZipPath(file.Name); err != nil {
			return fmt.Errorf("invalid file path in zip: %w", err)
		}

		path := filepath.Join(outputDir, file.Name)

		// Double-check that the resolved path is within outputDir
		absPath, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("failed to resolve path: %w", err)
		}
		absOutputDir, err := filepath.Abs(outputDir)
		if err != nil {
			return fmt.Errorf("failed to resolve output dir: %w", err)
		}
		if !strings.HasPrefix(absPath, absOutputDir+string(filepath.Separator)) && absPath != absOutputDir {
			return fmt.Errorf("invalid file path (path traversal attempt): %s", file.Name)
		}

		// Send file extraction message
		p.Send(fileExtractedMsg(file.Name))

		if file.FileInfo().IsDir() {
			os.MkdirAll(path, file.Mode())
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
		targetFile.Close()

		if err != nil {
			return err
		}
	}

	p.Send(downloadCompleteMsg{})
	return nil
}

func (d *Downloader) downloadToFile(ctx context.Context, file *os.File) error {
	return d.retryWithBackoff(ctx, "download file", func() error {
		req, err := http.NewRequestWithContext(ctx, "GET", d.url, nil)
		if err != nil {
			return err
		}

		resp, err := d.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

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
		r.limiter.WaitN(r.ctx, n)
		r.downloader.downloaded.Add(int64(n))
	}
	return n, err
}

// countingWriter tracks bytes written for TAR downloads
type countingWriter struct {
	writer     io.Writer
	downloader *Downloader
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	return n, err
}

// validateURLs performs pre-flight checks on all URLs
// Returns slice of part sizes and error if validation fails
func validateURLs(urls []string, client *http.Client) ([]int64, error) {
	partSizes := make([]int64, len(urls))

	for i, url := range urls {
		// 1. Check URL format
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return nil, fmt.Errorf("part %d: invalid URL format (must start with http:// or https://): %s", i+1, url)
		}

		// 2. HEAD request to get size and check range support
		resp, err := client.Head(url)
		if err != nil {
			return nil, fmt.Errorf("part %d: cannot reach URL: %v\nURL: %s", i+1, err, url)
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
		fmt.Fprintln(os.Stderr, "  -chunk N       Chunk size in bytes (default: 100MB)")
		fmt.Fprintln(os.Stderr, "  -output DIR    Output directory (creates if doesn't exist)")
		fmt.Fprintln(os.Stderr, "  -proxy URL     HTTP proxy URL (e.g., http://proxy:8080)")
		fmt.Fprintln(os.Stderr, "  -limit RATE    Bandwidth limit (e.g., 1M, 500K, 2.5M)")
		fmt.Fprintln(os.Stderr, "  -max-retries N Maximum retry attempts for failed connections (default: 10)")
		fmt.Fprintln(os.Stderr, "  -retry-delay D Initial retry delay (default: 1s)")
		fmt.Fprintln(os.Stderr, "  -format FMT    Force archive format (overrides auto-detection)")
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

	if isMultiPart {
		// Multi-part download path
		// Create HTTP client for validation
		transport := &http.Transport{
			MaxIdleConns:        1000,
			MaxIdleConnsPerHost: 1000,
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

		// Validate all URLs
		partSizes, err := validateURLs(urls, client)
		if err != nil {
			fmt.Fprintf(os.Stderr, "URL validation failed: %v\n", err)
			os.Exit(1)
		}

		// Print validation summary
		printValidationSummary(urls, partSizes)

		// Calculate total size
		for _, size := range partSizes {
			totalSize += size
		}

		// Create TUI with multi-part info
		model = initialModel(urls[0], totalSize, *verbose, true, len(urls))
	} else {
		// Single URL download path
		url := urls[0]
		downloader := NewDownloader(url, *workers, *chunkSize, *proxyURL, limitBytes, *verbose, *maxRetries, *retryDelay, memBytes)

		// Get file size first
		if err := downloader.getFileSize(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		totalSize = downloader.totalSize

		// Create TUI
		model = initialModel(url, downloader.totalSize, *verbose, false, 1)
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
			// Multi-part download
			// Re-get partSizes from validated URLs
			transport := &http.Transport{
				MaxIdleConns:        1000,
				MaxIdleConnsPerHost: 1000,
				MaxConnsPerHost:     0,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  true,
				ForceAttemptHTTP2:   true,
				DisableKeepAlives:   false,
				TLSHandshakeTimeout: 10 * time.Second,
			}
			if *proxyURL != "" {
				proxy, parseErr := neturl.Parse(*proxyURL)
				if parseErr == nil {
					transport.Proxy = http.ProxyURL(proxy)
				}
			}
			client := &http.Client{
				Transport: transport,
				Timeout:   30 * time.Second,
			}
			partSizes, validateErr := validateURLs(urls, client)
			if validateErr != nil {
				err = validateErr
			} else {
				err = downloadMultiPart(ctx, urls, partSizes, p, *outputDir, *workers, *chunkSize,
					*proxyURL, limitBytes, *verbose, *maxRetries, *retryDelay, memBytes, *archiveFormat)
			}
		} else {
			// Single URL download
			url := urls[0]
			downloader := NewDownloader(url, *workers, *chunkSize, *proxyURL, limitBytes, *verbose, *maxRetries, *retryDelay, memBytes)
			if sizeErr := downloader.getFileSize(); sizeErr != nil {
				err = sizeErr
			} else {
				err = downloader.Download(ctx, p, *outputDir, *archiveFormat)
			}
		}

		if err != nil {
			p.Send(errorMsg(err))
			errChan <- err
		} else {
			errChan <- nil
		}
	}()

	// Handle signals in background
	go func() {
		<-sigChan
		// Cancel context to stop download
		cancel()
		// Quit the TUI
		p.Quit()
	}()

	// Run TUI
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running TUI: %v\n", err)
		os.Exit(1)
	}

	// Check for download errors
	select {
	case err := <-errChan:
		if err != nil && err != context.Canceled {
			os.Exit(1)
		}
	default:
		// Download may have been interrupted by signal
	}
}
