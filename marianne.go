package main

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/time/rate"
)

// Retry configuration
const (
	defaultMaxRetries     = 10          // Maximum number of retry attempts
	defaultInitialDelay   = 1 * time.Second  // Initial retry delay
	defaultMaxDelay       = 30 * time.Second // Maximum retry delay
	defaultBackoffFactor  = 2.0         // Exponential backoff multiplier
)

const (
	defaultWorkers   = 8                  // Balanced worker count
	defaultChunkSize = 2 * 1024 * 1024    // 2MB chunks for better granularity
	maxMemoryBuffer  = 1024 * 1024 * 1024 // 1GB max memory buffer
)

var (
	// Version is set at build time
	Version = "dev"
)

// DownloadState stores the state of a download for resume
type DownloadState struct {
	URL            string       `json:"url"`
	TotalSize      int64        `json:"total_size"`
	Downloaded     int64        `json:"downloaded"`
	ArchiveType    string       `json:"archive_type"`
	OutputDir      string       `json:"output_dir"`
	TempFile       string       `json:"temp_file"`
	ChunkStates    []ChunkState `json:"chunk_states,omitempty"`
	ExtractedFiles []string     `json:"extracted_files,omitempty"`
	LastModified   string       `json:"last_modified"`
	ETag           string       `json:"etag"`
	Created        time.Time    `json:"created"`
	Updated        time.Time    `json:"updated"`
}

// ChunkState tracks individual chunk progress
type ChunkState struct {
	Index      int   `json:"index"`
	Start      int64 `json:"start"`
	End        int64 `json:"end"`
	Downloaded int64 `json:"downloaded"`
	Complete   bool  `json:"complete"`
}

type Downloader struct {
	url            string
	workers        int
	chunkSize      int64
	client         *http.Client
	totalSize      int64
	downloaded     atomic.Int64  // Use atomic for thread-safe counter
	startTime      time.Time
	rateLimiter    *rate.Limiter
	bandwidthLimit int64
	resumeFile     string
	stateFile      string
	state          *DownloadState
	verbose        bool
	maxRetries     int
	initialDelay   time.Duration
	maxDelay       time.Duration
	backoffFactor  float64
	mu             sync.Mutex
}

func NewDownloader(url string, workers int, chunkSize int64, proxyURL string, bandwidthLimit int64, verbose bool, maxRetries int, retryDelay time.Duration) *Downloader {
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
	
	err := d.retryWithBackoff(nil, "get file size", func() error {
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

	// Save ETag and Last-Modified for validation
	if d.state == nil {
		d.state = &DownloadState{
			URL:       d.url,
			TotalSize: size,
			Created:   time.Now(),
		}
	}
	d.state.ETag = resp.Header.Get("ETag")
	d.state.LastModified = resp.Header.Get("Last-Modified")

	// Check if server supports partial content (resume)
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		// Some servers don't advertise but still support ranges, we'll try anyway
	}

	return nil
}

func (d *Downloader) downloadChunk(ctx context.Context, start, end int64, writer io.Writer) error {
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

		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}

		buf := make([]byte, 1024*1024) // 1MB buffer for better performance
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
				// Don't update counter here - let parallel_download.go handle it
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
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

type chunkProgressMsg struct {
	chunkIndex int
	start      int64
	end        int64
	status     string // "started", "completed", "failed"
	workerID   int
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
}

func initialModel(url string, totalSize int64, verbose bool) model {
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
		m.viewport.Height = msg.Height - 12 // Leave room for progress and stats

	case progressMsg:
		m.downloaded = msg.downloaded
		m.speed = msg.speed
		m.eta = msg.eta
		if m.total > 0 {
			m.avgSpeed = float64(m.downloaded) / time.Since(m.startTime).Seconds()
		}

	case fileExtractedMsg:
		m.extractedFiles = append(m.extractedFiles, string(msg))
		content := strings.Join(m.extractedFiles, "\n")
		m.viewport.SetContent(content)
		m.viewport.GotoBottom()

	case chunkProgressMsg:
		if m.verbose {
			m.chunkProgress[msg.chunkIndex] = msg
			// Add chunk progress to extracted files for display
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
	header := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("211")).
		MarginBottom(1).
		Render(fmt.Sprintf("📥 Downloading: %s", m.url))

	// Progress bar
	prog := m.progress.ViewAs(float64(m.downloaded) / float64(m.total))

	// Stats
	statsText := fmt.Sprintf(
		"Progress: %.1f%% | Downloaded: %s/%s | Speed: %s/s | Avg: %s/s | ETA: %s",
		float64(m.downloaded)/float64(m.total)*100,
		formatBytes(m.downloaded),
		formatBytes(m.total),
		formatBytes(int64(m.speed)),
		formatBytes(int64(m.avgSpeed)),
		formatDuration(m.eta),
	)

	// Add chunk progress summary in verbose mode
	if m.verbose && len(m.chunkProgress) > 0 {
		activeChunks := 0
		completedChunks := 0
		for _, chunk := range m.chunkProgress {
			switch chunk.status {
			case "started":
				activeChunks++
			case "completed":
				completedChunks++
			}
		}
		statsText += fmt.Sprintf("\nChunks: %d active, %d completed", activeChunks, completedChunks)
	}

	stats := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		MarginTop(1).
		Render(statsText)

	// Extracted files section
	filesHeader := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("212")).
		MarginTop(2).
		MarginBottom(1).
		Render("📂 Extracted Files:")

	// Combine all elements
	return lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		prog,
		stats,
		filesHeader,
		m.viewport.View(),
	)
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

func (d *Downloader) Download(ctx context.Context, p *tea.Program, outputDir string, resume bool) error {
	// Set up state file
	d.stateFile = getStateFilename(d.url)

	// Try to load existing state if resuming
	if resume {
		if err := d.loadState(); err == nil {
			p.Send(fileExtractedMsg(fmt.Sprintf("Loaded state: %s downloaded", formatBytes(d.state.Downloaded))))

			// Validate the file hasn't changed
			oldETag := d.state.ETag
			oldLastModified := d.state.LastModified

			if err := d.getFileSize(); err != nil {
				return err
			}

			// Check if file has changed
			if (oldETag != "" && oldETag != d.state.ETag) ||
				(oldLastModified != "" && oldLastModified != d.state.LastModified) {
				p.Send(fileExtractedMsg("Warning: Remote file has changed, starting fresh download"))
				d.downloaded.Store(0)
				d.state.Downloaded = 0
				d.state.TempFile = ""
			}
		} else {
			// No valid state, get file info
			if err := d.getFileSize(); err != nil {
				return err
			}
		}
	} else {
		// Not resuming, get file info
		if err := d.getFileSize(); err != nil {
			return err
		}
	}

	d.startTime = time.Now()

	// Detect archive type from URL
	tarFlag, tarCommand, isZip, err := detectArchiveType(d.url)
	if err != nil {
		return err
	}

	if d.state != nil {
		d.state.ArchiveType = "tar"
		if isZip {
			d.state.ArchiveType = "zip"
		}
		d.state.OutputDir = outputDir
	}

	// Create output directory if it doesn't exist
	if outputDir != "" {
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	// Save initial state
	d.saveState()

	if isZip {
		// Handle ZIP files
		if d.state != nil && d.state.TempFile != "" {
			d.resumeFile = d.state.TempFile
		} else {
			// Generate resume filename from URL
			urlParts := strings.Split(d.url, "/")
			filename := urlParts[len(urlParts)-1]
			d.resumeFile = filepath.Join(os.TempDir(), fmt.Sprintf("marianne-resume-%s", filename))
			if d.state != nil {
				d.state.TempFile = d.resumeFile
				d.saveState()
			}
		}

		err := d.downloadAndExtractZip(ctx, p, outputDir, resume)
		if err == nil {
			d.cleanupState()
		}
		return err
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

	// If resuming and we have extracted files, skip existing files
	if resume && d.state != nil && len(d.state.ExtractedFiles) > 0 {
		args = append(args, "-k") // --keep-old-files: don't overwrite existing files
		p.Send(fileExtractedMsg(fmt.Sprintf("Resuming TAR extraction (found %d previously extracted files)", len(d.state.ExtractedFiles))))
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

	// Read tar output
	go func() {
		scanner := bufio.NewScanner(io.MultiReader(stdout, stderr))
		for scanner.Scan() {
			line := scanner.Text()
			if line != "" {
				p.Send(fileExtractedMsg(line))
				// Track extracted files in state
				if d.state != nil {
					d.state.ExtractedFiles = append(d.state.ExtractedFiles, line)
					// Save state periodically
					if len(d.state.ExtractedFiles)%50 == 0 {
						d.saveState()
					}
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

				// Calculate speed
				timeDiff := currentTime.Sub(lastTime).Seconds()
				if timeDiff > 0 {
					bytesDiff := currentDownloaded - lastDownloaded
					speed := float64(bytesDiff) / timeDiff

					// Calculate ETA
					remaining := d.totalSize - currentDownloaded
					var eta time.Duration
					if speed > 0 {
						eta = time.Duration(float64(remaining)/speed) * time.Second
					}

					p.Send(progressMsg{
						downloaded: currentDownloaded,
						total:      d.totalSize,
						speed:      speed,
						eta:        eta,
					})

					// Save state periodically (every 5 seconds)
					if d.state != nil && currentTime.Sub(d.state.Updated).Seconds() > 5 {
						d.saveState()
					}

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

	// Download chunks
	downloadErr := d.downloadInOrderParallel(ctx, pipeWriter, p)
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

// Generate state filename from URL
func getStateFilename(url string) string {
	h := sha256.Sum256([]byte(url))
	return filepath.Join(os.TempDir(), fmt.Sprintf("marianne-state-%s.json", hex.EncodeToString(h[:8])))
}

// Save download state
func (d *Downloader) saveState() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.state == nil {
		d.state = &DownloadState{
			URL:       d.url,
			TotalSize: d.totalSize,
			Created:   time.Now(),
		}
	}

	d.state.Downloaded = d.downloaded.Load()
	d.state.Updated = time.Now()

	data, err := json.MarshalIndent(d.state, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(d.stateFile, data, 0644)
}

// Load download state
func (d *Downloader) loadState() error {
	data, err := os.ReadFile(d.stateFile)
	if err != nil {
		return err
	}

	state := &DownloadState{}
	if err := json.Unmarshal(data, state); err != nil {
		return err
	}

	// Verify URL matches
	if state.URL != d.url {
		return fmt.Errorf("state file URL mismatch")
	}

	d.state = state
	d.downloaded.Store(state.Downloaded)
	return nil
}

// Clean up state file
func (d *Downloader) cleanupState() {
	if d.stateFile != "" {
		os.Remove(d.stateFile)
	}
}

func (d *Downloader) downloadAndExtractZip(ctx context.Context, p *tea.Program, outputDir string, resume bool) error {
	var tmpFile *os.File
	var err error
	var existingSize int64

	// Check for resume
	if resume && d.resumeFile != "" {
		// Try to resume from existing file
		if stat, err := os.Stat(d.resumeFile); err == nil {
			existingSize = stat.Size()
			if existingSize < d.totalSize {
				// Open for append
				tmpFile, err = os.OpenFile(d.resumeFile, os.O_RDWR, 0644)
				if err == nil {
					d.downloaded.Store(existingSize)
					p.Send(progressMsg{
						downloaded: existingSize,
						total:      d.totalSize,
						speed:      0,
						eta:        0,
					})
				}
			} else if existingSize == d.totalSize {
				// File already complete
				tmpFile, err = os.Open(d.resumeFile)
				if err != nil {
					return err
				}
				defer tmpFile.Close()
				// Skip download, go straight to extraction
				return d.extractZipFile(tmpFile.Name(), outputDir, p)
			}
		}
	}

	// If not resuming or resume failed, create new temp file
	if tmpFile == nil {
		tmpFile, err = os.CreateTemp("", "marianne-*.zip")
		if err != nil {
			return fmt.Errorf("failed to create temp file: %w", err)
		}
		d.resumeFile = tmpFile.Name()
		existingSize = 0
	}

	defer func() {
		tmpFile.Close()
		if !resume {
			os.Remove(tmpFile.Name())
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

				// Calculate speed
				timeDiff := currentTime.Sub(lastTime).Seconds()
				if timeDiff > 0 {
					bytesDiff := currentDownloaded - lastDownloaded
					speed := float64(bytesDiff) / timeDiff

					// Calculate ETA
					remaining := d.totalSize - currentDownloaded
					var eta time.Duration
					if speed > 0 {
						eta = time.Duration(float64(remaining)/speed) * time.Second
					}

					p.Send(progressMsg{
						downloaded: currentDownloaded,
						total:      d.totalSize,
						speed:      speed,
						eta:        eta,
					})

					// Save state periodically (every 5 seconds)
					if d.state != nil && currentTime.Sub(d.state.Updated).Seconds() > 5 {
						d.saveState()
					}

					lastDownloaded = currentDownloaded
					lastTime = currentTime
				}
			case <-done:
				return
			}
		}
	}()

	// Download to temp file (with resume if applicable)
	if existingSize < d.totalSize {
		if err := d.downloadToFileWithResume(ctx, tmpFile, existingSize); err != nil {
			close(done)
			return err
		}
	}
	close(done)

	// Extract ZIP file
	return d.extractZipFile(tmpFile.Name(), outputDir, p)
}

func (d *Downloader) extractZipFile(filename string, outputDir string, p *tea.Program) error {
	reader, err := zip.OpenReader(filename)
	if err != nil {
		return fmt.Errorf("failed to open zip file: %w", err)
	}
	defer reader.Close()

	// Extract files
	for _, file := range reader.File {
		path := filepath.Join(outputDir, file.Name)

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

		// Extract file
		fileReader, err := file.Open()
		if err != nil {
			return err
		}
		defer fileReader.Close()

		targetFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
		if err != nil {
			return err
		}
		defer targetFile.Close()

		_, err = io.Copy(targetFile, fileReader)
		if err != nil {
			return err
		}
	}

	p.Send(downloadCompleteMsg{})
	return nil
}

func (d *Downloader) downloadToFileWithResume(ctx context.Context, file *os.File, resumeFrom int64) error {
	// Seek to end if resuming
	if resumeFrom > 0 {
		if _, err := file.Seek(resumeFrom, 0); err != nil {
			return fmt.Errorf("failed to seek: %w", err)
		}
	}

	// Use buffered writer for better disk I/O performance
	bufWriter := bufio.NewWriterSize(file, 16*1024*1024) // 16MB buffer
	defer bufWriter.Flush()

	return d.retryWithBackoff(ctx, "download file", func() error {
		// Download entire file or remaining part
		req, err := http.NewRequestWithContext(ctx, "GET", d.url, nil)
		if err != nil {
			return err
		}

		// Add range header if resuming
		if resumeFrom > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
		}

		resp, err := d.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		// Check response status
		expectedStatus := http.StatusOK
		if resumeFrom > 0 {
			expectedStatus = http.StatusPartialContent
		}

		if resp.StatusCode != expectedStatus {
			// If we get 200 instead of 206 when resuming, server doesn't support resume
			if resumeFrom > 0 && resp.StatusCode == http.StatusOK {
				// Start from beginning
				d.downloaded.Store(0)
				if _, err := file.Seek(0, 0); err != nil {
					return err
				}
				if err := file.Truncate(0); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
			}
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

		_, err = io.Copy(bufWriter, reader)
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

func main() {
	var (
		workers        = flag.Int("workers", defaultWorkers, "Number of parallel workers")
		chunkSize      = flag.Int64("chunk", defaultChunkSize, "Chunk size in bytes")
		outputDir      = flag.String("output", "", "Output directory (creates if doesn't exist)")
		proxyURL       = flag.String("proxy", "", "HTTP proxy URL (e.g., http://proxy:8080)")
		bandwidthLimit = flag.String("limit", "", "Bandwidth limit (e.g., 1M, 500K, 2.5M)")
		showVersion    = flag.Bool("version", false, "Show version and exit")
		resume         = flag.Bool("resume", false, "Resume interrupted download (ZIP files only)")
		verbose        = flag.Bool("verbose", false, "Show detailed progress of individual chunks")
		memoryLimit    = flag.String("memory", "auto", "Memory limit for buffers (e.g., 1G, 500M, auto for 10% of system memory)")
		maxRetries     = flag.Int("max-retries", defaultMaxRetries, "Maximum number of retry attempts for failed connections")
		retryDelay     = flag.Duration("retry-delay", defaultInitialDelay, "Initial retry delay (e.g., 1s, 500ms)")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("Marianne %s\n", Version)
		fmt.Println("A blazing-fast parallel downloader with automatic archive extraction")
		fmt.Println("https://github.com/wesraph/marianne")
		os.Exit(0)
	}

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: marianne [options] <url>")
		fmt.Fprintln(os.Stderr, "\nOptions:")
		fmt.Fprintln(os.Stderr, "  -workers N     Number of parallel workers (default: 8)")
		fmt.Fprintln(os.Stderr, "  -chunk N       Chunk size in bytes (default: 100MB)")
		fmt.Fprintln(os.Stderr, "  -output DIR    Output directory (creates if doesn't exist)")
		fmt.Fprintln(os.Stderr, "  -proxy URL     HTTP proxy URL (e.g., http://proxy:8080)")
		fmt.Fprintln(os.Stderr, "  -limit RATE    Bandwidth limit (e.g., 1M, 500K, 2.5M)")
		fmt.Fprintln(os.Stderr, "  -resume        Resume interrupted download (ZIP files only)")
		fmt.Fprintln(os.Stderr, "  -max-retries N Maximum retry attempts for failed connections (default: 10)")
		fmt.Fprintln(os.Stderr, "  -retry-delay D Initial retry delay (default: 1s)")
		fmt.Fprintln(os.Stderr, "\nSupported archive formats:")
		fmt.Fprintln(os.Stderr, "  .zip, .tar, .tar.gz, .tgz, .tar.bz2, .tbz2, .tar.xz, .txz")
		fmt.Fprintln(os.Stderr, "  .tar.lz4, .tar.zst, .tar.zstd, .tar.lzma, .tar.Z")
		os.Exit(1)
	}

	url := flag.Arg(0)

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

	downloader := NewDownloader(url, *workers, *chunkSize, *proxyURL, limitBytes, *verbose, *maxRetries, *retryDelay)

	// Get file size first
	if err := downloader.getFileSize(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Create TUI
	model := initialModel(url, downloader.totalSize, *verbose)
	if *resume {
		// Check if state file exists
		stateFile := getStateFilename(url)
		if _, err := os.Stat(stateFile); err == nil {
			model.extractedFiles = append(model.extractedFiles,
				fmt.Sprintf("Resume state found at: %s", stateFile))
		}
	}
	p := tea.NewProgram(model)

	// Run download in background
	ctx := context.Background()
	errChan := make(chan error, 1)
	go func() {
		if err := downloader.Download(ctx, p, *outputDir, *resume); err != nil {
			p.Send(errorMsg(err))
			errChan <- err
		} else {
			errChan <- nil
		}
	}()

	// Run TUI
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running TUI: %v\n", err)
		os.Exit(1)
	}

	// Check for download errors
	if err := <-errChan; err != nil {
		os.Exit(1)
	}
}
