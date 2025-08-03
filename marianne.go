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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/time/rate"
)

const (
	defaultWorkers   = 8
	defaultChunkSize = 100 * 1024 * 1024 // 100MB chunks for better performance on large files
	maxMemoryBuffer  = 500 * 1024 * 1024 // 500MB max memory buffer
)

type Downloader struct {
	url           string
	workers       int
	chunkSize     int64
	client        *http.Client
	totalSize     int64
	downloaded    int64
	startTime     time.Time
	rateLimiter   *rate.Limiter
	bandwidthLimit int64
	mu            sync.Mutex
}

func NewDownloader(url string, workers int, chunkSize int64, proxyURL string, bandwidthLimit int64) *Downloader {
	transport := &http.Transport{}
	
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
			Timeout:   30 * time.Second,
		},
		bandwidthLimit: bandwidthLimit,
	}
	
	// Set up rate limiter if bandwidth limit is specified
	if bandwidthLimit > 0 {
		d.rateLimiter = rate.NewLimiter(rate.Limit(bandwidthLimit), int(bandwidthLimit))
	}
	
	return d
}

func (d *Downloader) getFileSize() error {
	resp, err := d.client.Head(d.url)
	if err != nil {
		return fmt.Errorf("failed to get file size: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to get file size: status %d", resp.StatusCode)
	}

	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return fmt.Errorf("failed to parse content length: %w", err)
	}

	d.totalSize = size
	return nil
}

func (d *Downloader) downloadChunk(ctx context.Context, start, end int64, writer io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, "GET", d.url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	buf := make([]byte, 32*1024) // 32KB buffer
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
			d.mu.Lock()
			d.downloaded += int64(n)
			d.mu.Unlock()
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	return nil
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
}

func initialModel(url string, totalSize int64) model {
	prog := progress.New(progress.WithDefaultGradient())
	vp := viewport.New(80, 10)
	vp.Style = lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		PaddingLeft(1).
		PaddingRight(1)

	return model{
		url:       url,
		progress:  prog,
		viewport:  vp,
		total:     totalSize,
		startTime: time.Now(),
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
	stats := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		MarginTop(1).
		Render(fmt.Sprintf(
			"Progress: %.1f%% | Downloaded: %s/%s | Speed: %s/s | Avg: %s/s | ETA: %s",
			float64(m.downloaded)/float64(m.total)*100,
			formatBytes(m.downloaded),
			formatBytes(m.total),
			formatBytes(int64(m.speed)),
			formatBytes(int64(m.avgSpeed)),
			formatDuration(m.eta),
		))

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

func (d *Downloader) Download(ctx context.Context, p *tea.Program, outputDir string) error {
	if err := d.getFileSize(); err != nil {
		return err
	}

	d.startTime = time.Now()

	// Detect archive type from URL
	tarFlag, tarCommand, isZip, err := detectArchiveType(d.url)
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

	// Read tar output
	go func() {
		scanner := bufio.NewScanner(io.MultiReader(stdout, stderr))
		for scanner.Scan() {
			line := scanner.Text()
			if line != "" {
				p.Send(fileExtractedMsg(line))
			}
		}
	}()

	// Progress reporter
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		lastDownloaded := int64(0)
		lastTime := time.Now()
		
		for {
			select {
			case <-ticker.C:
				d.mu.Lock()
				currentDownloaded := d.downloaded
				currentTime := time.Now()
				d.mu.Unlock()
				
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
	downloadErr := d.downloadInOrder(ctx, pipeWriter)
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

type chunkData struct {
	data []byte
	err  error
}

func (d *Downloader) downloadInOrder(ctx context.Context, writer io.Writer) error {
	var offset int64
	
	// Create a pipeline with limited buffer
	chunkChan := make(chan chunkData, 2) // Buffer only 2 chunks ahead
	
	// Start writer goroutine
	writerDone := make(chan error, 1)
	go func() {
		for chunk := range chunkChan {
			if chunk.err != nil {
				writerDone <- chunk.err
				return
			}
			if _, err := writer.Write(chunk.data); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()
	
	// Download and stream chunks
	for offset < d.totalSize {
		chunkSize := d.chunkSize
		if offset+chunkSize > d.totalSize {
			chunkSize = d.totalSize - offset
		}

		// For very large chunks, download in smaller sub-chunks to avoid memory issues
		if chunkSize > maxMemoryBuffer {
			// Download in maxMemoryBuffer-sized pieces
			subOffset := offset
			for subOffset < offset+chunkSize {
				subChunkSize := int64(maxMemoryBuffer)
				if subOffset+subChunkSize > offset+chunkSize {
					subChunkSize = offset + chunkSize - subOffset
				}
				
				if err := d.downloadSingleChunk(ctx, subOffset, subOffset+subChunkSize-1, chunkChan); err != nil {
					close(chunkChan)
					<-writerDone
					return err
				}
				
				subOffset += subChunkSize
			}
		} else {
			// Download chunks in parallel but write in order
			numWorkers := d.workers
			workerChunkSize := chunkSize / int64(numWorkers)
			if workerChunkSize < 1024*1024 { // At least 1MB per worker
				numWorkers = int(chunkSize / (1024 * 1024))
				if numWorkers < 1 {
					numWorkers = 1
				}
				workerChunkSize = chunkSize / int64(numWorkers)
			}

			chunks := make([][]byte, numWorkers)
			var wg sync.WaitGroup
			errChan := make(chan error, numWorkers)

			for i := 0; i < numWorkers; i++ {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					
					start := offset + int64(idx)*workerChunkSize
					end := start + workerChunkSize - 1
					if idx == numWorkers-1 {
						end = offset + chunkSize - 1
					}

					buf := make([]byte, 0, end-start+1)
					bufWriter := &appendWriter{buf: &buf}
					
					if err := d.downloadChunk(ctx, start, end, bufWriter); err != nil {
						errChan <- err
						return
					}
					
					chunks[idx] = buf
				}(i)
			}

			wg.Wait()
			close(errChan)

			// Check for errors
			for err := range errChan {
				if err != nil {
					close(chunkChan)
					<-writerDone
					return err
				}
			}

			// Send chunks to writer in order
			for _, chunk := range chunks {
				select {
				case chunkChan <- chunkData{data: chunk}:
				case <-ctx.Done():
					close(chunkChan)
					<-writerDone
					return ctx.Err()
				}
			}
		}

		offset += chunkSize
	}
	
	close(chunkChan)
	return <-writerDone
}

func (d *Downloader) downloadSingleChunk(ctx context.Context, start, end int64, chunkChan chan<- chunkData) error {
	req, err := http.NewRequestWithContext(ctx, "GET", d.url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Stream directly without buffering entire chunk
	buf := make([]byte, 1024*1024) // 1MB buffer for streaming
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			// Apply rate limiting if configured
			if d.rateLimiter != nil {
				d.rateLimiter.WaitN(ctx, n)
			}
			
			data := make([]byte, n)
			copy(data, buf[:n])
			
			select {
			case chunkChan <- chunkData{data: data}:
				d.mu.Lock()
				d.downloaded += int64(n)
				d.mu.Unlock()
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	return nil
}

type appendWriter struct {
	buf *[]byte
}

func (w *appendWriter) Write(p []byte) (n int, err error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
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

func (d *Downloader) downloadAndExtractZip(ctx context.Context, p *tea.Program, outputDir string) error {
	// Create a temporary file for the ZIP
	tmpFile, err := os.CreateTemp("", "marianne-*.zip")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Progress reporter
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		lastDownloaded := int64(0)
		lastTime := time.Now()
		
		for {
			select {
			case <-ticker.C:
				d.mu.Lock()
				currentDownloaded := d.downloaded
				currentTime := time.Now()
				d.mu.Unlock()
				
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
	reader, err := zip.OpenReader(tmpFile.Name())
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

func (d *Downloader) downloadToFile(ctx context.Context, file *os.File) error {
	// Download entire file
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
			reader:      resp.Body,
			limiter:     d.rateLimiter,
			ctx:         ctx,
			downloader:  d,
		}
	}

	_, err = io.Copy(file, reader)
	return err
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
		r.downloader.mu.Lock()
		r.downloader.downloaded += int64(n)
		r.downloader.mu.Unlock()
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
	)
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: marianne [options] <url>")
		fmt.Fprintln(os.Stderr, "\nOptions:")
		fmt.Fprintln(os.Stderr, "  -workers N     Number of parallel workers (default: 8)")
		fmt.Fprintln(os.Stderr, "  -chunk N       Chunk size in bytes (default: 100MB)")
		fmt.Fprintln(os.Stderr, "  -output DIR    Output directory (creates if doesn't exist)")
		fmt.Fprintln(os.Stderr, "  -proxy URL     HTTP proxy URL (e.g., http://proxy:8080)")
		fmt.Fprintln(os.Stderr, "  -limit RATE    Bandwidth limit (e.g., 1M, 500K, 2.5M)")
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
	
	downloader := NewDownloader(url, *workers, *chunkSize, *proxyURL, limitBytes)

	// Get file size first
	if err := downloader.getFileSize(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Create TUI
	p := tea.NewProgram(initialModel(url, downloader.totalSize))

	// Run download in background
	ctx := context.Background()
	errChan := make(chan error, 1)
	go func() {
		if err := downloader.Download(ctx, p, *outputDir); err != nil {
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