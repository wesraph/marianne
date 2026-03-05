package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	maxChunksInMemoryCap = 64          // Maximum chunks to buffer in memory
	defaultMemoryLimit   = 1024 * 1024 * 1024 // 1GB default memory limit
	memoryUsageFraction  = 0.8         // Use 80% of memory limit for chunks
)

// downloadInOrderParallel downloads chunks in parallel and writes them in order
func (d *Downloader) downloadInOrderParallel(ctx context.Context, writer io.Writer, p *tea.Program) error {
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
	// Reserve some memory for other overhead
	usableMemory := int64(float64(memoryLimit) * memoryUsageFraction)
	maxChunksInMemory := int(usableMemory / d.chunkSize)
	if maxChunksInMemory < d.workers*2 {
		// Ensure at least 2 chunks per worker
		maxChunksInMemory = d.workers * 2
	}
	if maxChunksInMemory > maxChunksInMemoryCap {
		// Cap to prevent excessive buffering
		maxChunksInMemory = maxChunksInMemoryCap
	}
	
	// Channels for coordination with backpressure
	downloadQueue := make(chan chunkInfo, d.workers*2) // Queue for workers
	// Large buffer to prevent worker blocking when chunks arrive out of order
	// This prevents deadlock where workers block on send while writer waits for next chunk
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

				// Download the chunk
				var buf bytes.Buffer
				if err := d.downloadChunkWithProgress(ctx, chunk.start, chunk.end, &buf, p, chunk.index, workerID, startTime); err != nil {
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
					resultChan <- chunk
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
				resultChan <- chunk
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

			// Queue for download with context checking
			// The channel buffer size provides natural backpressure
			select {
			case downloadQueue <- chunkInfo{index: i, start: start, end: end}:
				// Chunk queued successfully
			case <-ctx.Done():
				return
			}

			// Small delay every 10 chunks to allow some processing time
			if i%10 == 0 && i > 0 {
				timer := time.NewTimer(5 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
					// Timer fired
				}
				timer.Stop() // Release timer resources
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
					// Note: When ok==false, the channel is already drained (all buffered items received)
					// All chunks should be in pendingChunks map at this point
					for chunksWritten < totalChunks {
						if data, ok := pendingChunks[nextExpectedChunk]; ok {
							if len(data) > 0 {
								// CRITICAL: Check actual bytes written
								n, err := writer.Write(data)
								if err != nil {
									writerErr <- fmt.Errorf("failed to write chunk %d: %w", nextExpectedChunk, err)
									return
								}
								// Detect short writes
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
							// Missing chunks - channel closed but we don't have all chunks
							writerErr <- fmt.Errorf("result channel closed before all chunks received: %d/%d", chunksWritten, totalChunks)
							return
						}
					}
					// All chunks written successfully
					writerErr <- nil
					return
				}

				if chunk.err != nil {
					// Return error immediately - the channel will be drained
					// after downloadWg.Wait() completes and closes resultChan
					writerErr <- chunk.err
					return
				}

				// Store all chunks for sequential writing
				// Note: We removed the strict memory limit check here to prevent deadlock
				// The large resultChan buffer (maxChunksInMemory) provides backpressure instead
				if len(chunk.data) > 0 {
					pendingChunks[chunk.index] = chunk.data
				} else if len(chunk.data) == 0 {
					// Chunk downloaded but has zero bytes - this might indicate an error
					// Store it anyway to maintain chunk count, but it won't be written
					pendingChunks[chunk.index] = chunk.data
				}

				// Write any consecutive chunks we can
				for {
					if data, ok := pendingChunks[nextExpectedChunk]; ok {
						if len(data) > 0 {
							// CRITICAL: Check actual bytes written, not assumed
							n, err := writer.Write(data)
							if err != nil {
								writerErr <- fmt.Errorf("failed to write chunk %d: %w", nextExpectedChunk, err)
								return
							}
							// Detect short writes (writer didn't write all bytes)
							if n != len(data) {
								writerErr <- fmt.Errorf("short write on chunk %d: wrote %d bytes, expected %d", nextExpectedChunk, n, len(data))
								return
							}
							// Update progress based on ACTUAL bytes written
							d.downloaded.Add(int64(n))
						}
						delete(pendingChunks, nextExpectedChunk)
						nextExpectedChunk++
						chunksWritten++
					} else {
						break
					}
				}
			case <-ctx.Done():
				writerErr <- ctx.Err()
				return
			}
		}
		
		writerErr <- nil
	}()
	
	// Wait for downloads to complete
	downloadWg.Wait()
	close(resultChan)

	// Wait for writer to complete
	err := <-writerErr
	if err != nil {
		return err
	}

	// Verify total bytes written matches expected size
	totalDownloaded := d.downloaded.Load()
	if totalDownloaded != d.totalSize {
		return fmt.Errorf("download size mismatch: expected %d bytes, got %d bytes (possible corruption)", d.totalSize, totalDownloaded)
	}

	return nil
}