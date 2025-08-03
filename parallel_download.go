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
	
	// Limit memory usage - max chunks in flight
	maxChunksInMemory := 32 // At 5MB per chunk, this is ~160MB max
	
	// Track chunks that have been queued
	queuedChunks := 0
	
	// Channels for coordination with backpressure
	downloadQueue := make(chan chunkInfo, d.workers) // Only queue as many as workers
	resultChan := make(chan chunkInfo, maxChunksInMemory)
	
	// Start download workers
	var downloadWg sync.WaitGroup
	for i := 0; i < d.workers; i++ {
		downloadWg.Add(1)
		go func(workerID int) {
			defer downloadWg.Done()
			
			for chunk := range downloadQueue {
				// Send progress message if verbose
				if d.verbose && p != nil {
					p.Send(chunkProgressMsg{
						chunkIndex: chunk.index,
						start:      chunk.start,
						end:        chunk.end,
						status:     "started",
						workerID:   workerID,
					})
				}
				
				// Download the chunk
				var buf bytes.Buffer
				if err := d.downloadChunk(ctx, chunk.start, chunk.end, &buf); err != nil {
					chunk.err = fmt.Errorf("worker %d failed to download chunk %d: %w", workerID, chunk.index, err)
					if d.verbose && p != nil {
						p.Send(chunkProgressMsg{
							chunkIndex: chunk.index,
							start:      chunk.start,
							end:        chunk.end,
							status:     "failed",
							workerID:   workerID,
						})
					}
					resultChan <- chunk
					continue
				}
				
				chunk.data = buf.Bytes()
				if d.verbose && p != nil {
					p.Send(chunkProgressMsg{
						chunkIndex: chunk.index,
						start:      chunk.start,
						end:        chunk.end,
						status:     "completed",
						workerID:   workerID,
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
			
			// Skip if already downloaded (for resume support)
			if d.state != nil && i < len(d.state.ChunkStates) && d.state.ChunkStates[i].Complete {
				// Send empty chunk directly to result channel
				select {
				case resultChan <- chunkInfo{index: i, data: []byte{}}:
				case <-ctx.Done():
					return
				}
				continue
			}
			
			// Queue for download with context checking
			select {
			case downloadQueue <- chunkInfo{index: i, start: start, end: end}:
				queuedChunks++
			case <-ctx.Done():
				return
			}
			
			// Add small delay to prevent overwhelming the system
			if i%100 == 0 && i > 0 {
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()
	
	// Writer goroutine that ensures chunks are written in order
	writerErr := make(chan error, 1)
	go func() {
		defer close(writerErr)
		
		// Use a map to buffer out-of-order chunks with limited size
		pendingChunks := make(map[int][]byte)
		nextExpectedChunk := 0
		chunksWritten := 0
		
		for chunksWritten < totalChunks {
			select {
			case chunk := <-resultChan:
				if chunk.err != nil {
					writerErr <- chunk.err
					return
				}
				
				// Store the chunk data if not empty
				if len(chunk.data) > 0 {
					pendingChunks[chunk.index] = chunk.data
				}
				
				// Write any consecutive chunks we can
				for {
					if data, ok := pendingChunks[nextExpectedChunk]; ok {
						if len(data) > 0 {
							if _, err := writer.Write(data); err != nil {
								writerErr <- fmt.Errorf("failed to write chunk %d: %w", nextExpectedChunk, err)
								return
							}
						}
						delete(pendingChunks, nextExpectedChunk)
						nextExpectedChunk++
						chunksWritten++
						
						// Update progress more accurately
						d.mu.Lock()
						d.downloaded = int64(nextExpectedChunk) * d.chunkSize
						if d.downloaded > d.totalSize {
							d.downloaded = d.totalSize
						}
						d.mu.Unlock()
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
	return <-writerErr
}