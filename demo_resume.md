# Resume Functionality Demo

The marianne downloader now supports graceful shutdown and resume with the following features:

## Features Implemented

1. **SIGTERM Signal Handling**: Captures SIGTERM and SIGINT signals for graceful shutdown
2. **State File Creation**: Creates a state file (`.marianne-state-<hash>.json`) in the current directory
3. **Byte Position Tracking**: Tracks exact byte position for accurate resume
4. **Buffer Flushing**: Properly flushes write buffers before saving state
5. **File Sync**: Ensures data is written to disk before shutdown

## How It Works

### On Interrupt (SIGTERM/SIGINT):
1. Signal handler is triggered
2. Write buffer is flushed to disk
3. File is synced to ensure all data is persisted
4. Current byte position is captured
5. State is saved to `.marianne-state-<hash>.json`
6. User is notified with resume command

### State File Contents:
```json
{
  "url": "download_url",
  "total_size": 1234567,
  "downloaded": 500000,
  "byte_position": 500000,  // Exact byte position for resume
  "archive_type": "tar/zip",
  "output_dir": "output",
  "temp_file": "temp_file_path",
  "last_modified": "header_value",
  "etag": "header_value",
  "created": "timestamp",
  "updated": "timestamp"
}
```

### To Resume:
```bash
./marianne -resume <url>
```

The downloader will:
1. Load the state file from current directory
2. Verify the remote file hasn't changed (ETag/Last-Modified)
3. Seek to the saved byte position
4. Continue downloading from that exact position
5. Clean up state file on successful completion

## Implementation Details

- State files are created in the current working directory with pattern `.marianne-state-<hash>.json`
- The hash is based on the URL to ensure unique state files per download
- Both ZIP and TAR downloads are supported for resume
- Chunk-level resume is supported for parallel downloads
- Write buffers are properly tracked and flushed on interrupt

## Testing

To test the resume functionality:

1. Start a download:
```bash
./marianne -verbose <url>
```

2. Interrupt with Ctrl+C or kill -TERM <pid>

3. Resume the download:
```bash
./marianne -resume <url>
```

The download will continue from the exact byte position where it was interrupted.