package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	maxChunksInMemoryCap = 64                     // Maximum chunks to buffer in memory
	defaultMemoryLimit   = 1024 * 1024 * 1024     // 1GB default memory limit
	memoryUsageFraction  = 0.8                     // Use 80% of memory limit for chunks
)

// downloadInOrderParallel downloads chunks in parallel and writes them in order
func (d *Downloader) downloadInOrderParallel(ctx context.Context, writer io.Writer, p *tea.Program) error {
	// Reset download counter for accurate size verification
	d.downloaded.Store(0)

	// Calculate total chunks
	totalChunks := int((d.totalSize + d.chunkSize - 1) / d.chunkSize)

	// Chunk info for tracking
	type chunkInfo struct {
		index int
		start int64
		end   int64
		data  []byte
		err   error
	}

	// Calculate memory limits based on configured memory limit
	memoryLimit := d.memoryLimit
	if memoryLimit <= 0 {
		memoryLimit = defaultMemoryLimit
	}

	// Calculate max chunks that can fit in memory
	// Reserve some memory for other overhead (HTTP buffers, TLS, goroutine stacks)
	usableMemory := int64(float64(memoryLimit) * memoryUsageFraction)
	maxChunksInMemory := int(usableMemory / d.chunkSize)
	if maxChunksInMemory < 2 {
		// Absolute minimum: need at least 2 chunks to make progress
		maxChunksInMemory = 2
	}
	if maxChunksInMemory > maxChunksInMemoryCap {
		// Cap to prevent excessive buffering
		maxChunksInMemory = maxChunksInMemoryCap
	}

	// Inner context: cancelled when the writer hits an error, so workers unblock
	innerCtx, innerCancel := context.WithCancel(ctx)
	defer innerCancel()

	// Flow control: tracks next chunk index the writer needs to write.
	// The producer won't queue chunks more than maxChunksInMemory ahead of this.
	var nextWritten atomic.Int64

	// Channels for coordination with backpressure
	downloadQueue := make(chan chunkInfo, d.workers*2) // Queue for workers
	resultChan := make(chan chunkInfo, maxChunksInMemory)

	// Start download workers
	var downloadWg sync.WaitGroup
	for i := 0; i < d.workers; i++ {
		downloadWg.Add(1)
		go func(workerID int) {
			defer downloadWg.Done()

			for chunk := range downloadQueue {
				startTime := time.Now()

				// Send progress message if verbose
				if p != nil {
					p.Send(chunkProgressMsg{
						chunkIndex: chunk.index,
						start:      chunk.start,
						end:        chunk.end,
						status:     "started",
						workerID:   workerID,
						startTime:  startTime,
					})
				}

				// Download the chunk with pre-allocated buffer
				expectedSize := int(chunk.end - chunk.start + 1)
				var buf bytes.Buffer
				buf.Grow(expectedSize)
				if err := d.downloadChunkWithProgress(innerCtx, chunk.start, chunk.end, &buf, p, chunk.index, workerID, startTime); err != nil {
					chunk.err = fmt.Errorf("worker %d failed to download chunk %d: %w", workerID, chunk.index, err)
					if p != nil {
						p.Send(chunkProgressMsg{
							chunkIndex: chunk.index,
							start:      chunk.start,
							end:        chunk.end,
							status:     "failed",
							workerID:   workerID,
							startTime:  startTime,
						})
					}
					select {
					case resultChan <- chunk:
					case <-innerCtx.Done():
						return
					}
					continue
				}

				chunk.data = buf.Bytes()

				// Calculate download speed
				elapsed := time.Since(startTime).Seconds()
				chunkSize := float64(chunk.end - chunk.start + 1)
				speed := 0.0
				if elapsed > 0 {
					speed = chunkSize / elapsed
				}

				if p != nil {
					p.Send(chunkProgressMsg{
						chunkIndex:      chunk.index,
						start:           chunk.start,
						end:             chunk.end,
						status:          "completed",
						workerID:        workerID,
						startTime:       startTime,
						speed:           speed,
						bytesDownloaded: int64(len(chunk.data)),
					})
				}
				select {
				case resultChan <- chunk:
				case <-innerCtx.Done():
					return
				}
			}
		}(i)
	}

	// Chunk producer goroutine - feeds work to downloaders with flow control
	go func() {
		defer close(downloadQueue)

		for i := 0; i < totalChunks; i++ {
			start := int64(i) * d.chunkSize
			end := start + d.chunkSize - 1
			if end >= d.totalSize {
				end = d.totalSize - 1
			}

			// Flow control: don't get too far ahead of the writer.
			// This bounds total in-flight chunk data to ~maxChunksInMemory * chunkSize.
			for {
				written := nextWritten.Load()
				if int64(i)-written < int64(maxChunksInMemory) {
					break
				}
				timer := time.NewTimer(10 * time.Millisecond)
				select {
				case <-innerCtx.Done():
					timer.Stop()
					return
				case <-timer.C:
					timer.Stop()
				}
			}

			// Queue for download with context checking
			select {
			case downloadQueue <- chunkInfo{index: i, start: start, end: end}:
				// Chunk queued successfully
			case <-innerCtx.Done():
				return
			}
		}
	}()

	// Writer goroutine that ensures chunks are written in order
	writerErr := make(chan error, 1)
	go func() {
		defer close(writerErr)

		// Use a map to buffer out-of-order chunks
		pendingChunks := make(map[int][]byte)
		nextExpectedChunk := 0
		chunksWritten := 0

		for chunksWritten < totalChunks {
			select {
			case chunk, ok := <-resultChan:
				if !ok {
					// Channel closed - write any remaining pending chunks in order
					for chunksWritten < totalChunks {
						if data, ok := pendingChunks[nextExpectedChunk]; ok {
							if len(data) > 0 {
								n, err := writer.Write(data)
								if err != nil {
									writerErr <- fmt.Errorf("failed to write chunk %d: %w", nextExpectedChunk, err)
									return
								}
								if n != len(data) {
									writerErr <- fmt.Errorf("short write on chunk %d: wrote %d bytes, expected %d", nextExpectedChunk, n, len(data))
									return
								}
								d.downloaded.Add(int64(n))
							}
							delete(pendingChunks, nextExpectedChunk)
							nextExpectedChunk++
							chunksWritten++
						} else {
							writerErr <- fmt.Errorf("result channel closed before all chunks received: %d/%d", chunksWritten, totalChunks)
							return
						}
					}
					writerErr <- nil
					return
				}

				if chunk.err != nil {
					// Cancel inner context so workers and producer unblock
					innerCancel()
					writerErr <- chunk.err
					return
				}

				// Store chunk for sequential writing
				pendingChunks[chunk.index] = chunk.data

				// Write any consecutive chunks we can
				for {
					if data, ok := pendingChunks[nextExpectedChunk]; ok {
						if len(data) > 0 {
							n, err := writer.Write(data)
							if err != nil {
								innerCancel()
								writerErr <- fmt.Errorf("failed to write chunk %d: %w", nextExpectedChunk, err)
								return
							}
							if n != len(data) {
								innerCancel()
								writerErr <- fmt.Errorf("short write on chunk %d: wrote %d bytes, expected %d", nextExpectedChunk, n, len(data))
								return
							}
							d.downloaded.Add(int64(n))
						}
						delete(pendingChunks, nextExpectedChunk)
						nextExpectedChunk++
						chunksWritten++
						// Update flow control counter so producer can advance
						nextWritten.Store(int64(nextExpectedChunk))
					} else {
						break
					}
				}
			case <-innerCtx.Done():
				writerErr <- innerCtx.Err()
				return
			}
		}

		writerErr <- nil
	}()

	// The writer goroutine is the primary consumer of resultChan.
	// If it exits early (error), workers may block on resultChan send.
	// Drain resultChan in background after writer exits so workers can finish.
	var writeResult error
	var drainWg sync.WaitGroup
	drainWg.Add(1)
	go func() {
		defer drainWg.Done()
		writeResult = <-writerErr
		// Drain any remaining results so workers can unblock and exit
		for range resultChan {
		}
	}()

	// Wait for all workers to finish, then close resultChan so drainer exits
	downloadWg.Wait()
	close(resultChan)

	// Wait for drainer to finish
	drainWg.Wait()

	if writeResult != nil {
		return writeResult
	}

	// Verify total bytes written matches expected size
	totalDownloaded := d.downloaded.Load()
	if totalDownloaded != d.totalSize {
		return fmt.Errorf("download size mismatch: expected %d bytes, got %d bytes (possible corruption)", d.totalSize, totalDownloaded)
	}

	return nil
}
