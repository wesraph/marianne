<div align="center">
  <img src="logo.svg" alt="Marianne Logo" width="200" height="200">
  
  # 🚀 Marianne

  A blazing-fast parallel downloader with automatic archive extraction and a beautiful terminal UI. Download large files at maximum speed using concurrent connections and extract them on-the-fly.
</div>

<div align="center">

![Go Version](https://img.shields.io/badge/Go-1.25.3%2B-blue)
![License](https://img.shields.io/badge/license-MIT-green)
![Platform](https://img.shields.io/badge/platform-linux%20%7C%20macos%20%7C%20windows-lightgrey)
[![CI](https://github.com/wesraph/marianne/actions/workflows/ci.yml/badge.svg)](https://github.com/wesraph/marianne/actions/workflows/ci.yml)
[![Release](https://github.com/wesraph/marianne/actions/workflows/release.yml/badge.svg)](https://github.com/wesraph/marianne/actions/workflows/release.yml)

</div>

## ✨ Features

- **⚡ Parallel Downloads**: Split files into chunks and download them concurrently for maximum speed
- **🔗 Multi-Link Archives**: Download multi-part archives from multiple URLs and extract as one
- **📊 Beautiful TUI**: Real-time progress bar, download stats, and live file extraction view
- **🗜️ Auto-extraction**: Automatically detects and extracts various archive formats (ZIP, TAR, etc.)
- **🎯 Format Override**: Force specific archive format with `--format` flag
- **💾 Memory Efficient**: Automatic memory management with configurable limits
- **🔄 Retry Logic**: Exponential backoff retry for robust downloads over unreliable connections
- **📁 Output Control**: Specify output directory with automatic creation
- **🎯 Smart Chunking**: Optimized chunk sizes based on available memory
- **🌐 Proxy Support**: HTTP proxy support for corporate networks
- **⏱️ Rate Limiting**: Bandwidth limiting to avoid network congestion
- **📝 Verbose Mode**: Detailed chunk-level progress for debugging

## 🏗️ How It Works

Marianne uses a sophisticated parallel downloading architecture:

1. **File Size Detection**: Performs a HEAD request to determine the total file size
2. **Chunk Planning**: Divides the file into chunks based on configured chunk size
3. **Parallel Workers**: Spawns multiple workers (default: 8) to download chunks concurrently
4. **In-Order Assembly**: Downloaded chunks are buffered and written in sequence to maintain file integrity
5. **Memory Management**: Limits buffered chunks based on available memory to prevent OOM
6. **Stream Extraction**: For TAR files, pipes data directly to `tar` command for on-the-fly extraction
7. **Temp File Extraction**: For ZIP files, downloads to temp file then extracts (enables future resume support)

### Key Components

- **Worker Pool**: Concurrent goroutines download chunks in parallel
- **Rate Limiter**: Optional bandwidth throttling using token bucket algorithm
- **Retry Logic**: Exponential backoff for transient network failures
- **Progress Tracking**: Real-time UI updates using Bubble Tea framework
- **Chunk Coordinator**: Ensures chunks are written in correct order despite parallel downloads

## 📸 Screenshot

```
📥 Downloading: https://example.com/archive.tar.lz4

███████████████████░░░░░░░░░░░░░░░░░░░░░░  45.2%

Progress: 45.2% | Downloaded: 2.3 GB/5.1 GB | Speed: 25.4 MB/s | Avg: 22.1 MB/s | ETA: 1m54s

📂 Extracted Files:
╭───────────────────────────────────────────╮
│ data/file1.txt                            │
│ data/file2.json                           │
│ data/images/photo.jpg                     │
│ data/docs/readme.pdf                      │
╰───────────────────────────────────────────╯
```

## 🆕 What's New

- **Multi-Link Archives**: Download archives split across multiple URLs (e.g., part1, part2, part3)
- **Format Override**: Force archive type with `--format` flag instead of auto-detection
- **ZIP Support**: Now supports ZIP file extraction alongside TAR archives
- **HTTP Proxy**: Connect through HTTP proxies with `-proxy` flag
- **Bandwidth Limiting**: Control download speed with `-limit` flag
- **Memory Management**: Automatic memory limit detection with `-memory` flag
- **Retry Logic**: Configurable retry attempts with exponential backoff
- **Verbose Mode**: Detailed chunk-level progress tracking with `-verbose` flag

## 🔧 Installation

### Download Pre-built Binaries

Download the latest release for your platform from the [releases page](https://github.com/wesraph/marianne/releases).

```bash
# Linux (AMD64)
wget https://github.com/wesraph/marianne/releases/latest/download/marianne-linux-amd64
chmod +x marianne-linux-amd64
sudo mv marianne-linux-amd64 /usr/local/bin/marianne

# macOS (Intel)
wget https://github.com/wesraph/marianne/releases/latest/download/marianne-darwin-amd64
chmod +x marianne-darwin-amd64
sudo mv marianne-darwin-amd64 /usr/local/bin/marianne

# macOS (Apple Silicon)
wget https://github.com/wesraph/marianne/releases/latest/download/marianne-darwin-arm64
chmod +x marianne-darwin-arm64
sudo mv marianne-darwin-arm64 /usr/local/bin/marianne
```

### From Source

```bash
# Clone the repository
git clone https://github.com/wesraph/marianne.git
cd marianne

# Build the binary
make build

# Or build a static binary (recommended for portability)
make static
```

### Requirements

- Go 1.25.3 or higher (for building from source)
- `tar` command (pre-installed on most Unix systems)
- For `.tar.lz4`: `lz4` command (install via package manager)
- For `.tar.zst`: `zstd` command (install via package manager)

**Note**: Pre-built binaries have no runtime dependencies except for the decompression tools needed for the specific archive format you're downloading.

## 📖 Usage

### Basic Usage

```bash
# Download single archive
./marianne https://example.com/large-file.tar.gz

# Download multi-part archive (multiple URLs)
./marianne https://example.com/part1.tar.gz https://example.com/part2.tar.gz https://example.com/part3.tar.gz

# Force archive format (override auto-detection)
./marianne --format tar.gz https://example.com/file-without-extension
```

### With Options

```bash
# Download to specific directory
./marianne -output /path/to/extract https://example.com/archive.tar.xz

# Customize workers and chunk size
./marianne -workers 16 -chunk 209715200 https://example.com/huge.tar.lz4

# Use HTTP proxy
./marianne -proxy http://proxy.company.com:8080 https://example.com/file.zip

# Limit bandwidth to 2.5 MB/s
./marianne -limit 2.5M https://example.com/large-file.tar.gz

# Show detailed chunk progress
./marianne -verbose https://example.com/archive.tar.gz

# Set memory limit for buffering
./marianne -memory 2G https://example.com/huge.tar.lz4

# Configure retry behavior
./marianne -max-retries 5 -retry-delay 2s https://example.com/unreliable-server.zip

# Download multi-part archive with format override
./marianne --format tar.xz https://cdn.example.com/data.part1 https://cdn.example.com/data.part2

# Combine options
./marianne -output /data -workers 16 -memory 4G -verbose -limit 10M https://example.com/archive.zip
```

### Options

| Flag | Description | Default |
|------|-------------|---------|
| `-workers` | Number of parallel download workers | 8 |
| `-chunk` | Chunk size in bytes | 2097152 (2MB) |
| `-output` | Output directory (creates if doesn't exist) | Current directory |
| `-format` | Force archive format (zip, tar, tar.gz, tar.bz2, tar.xz, tar.lz4, tar.zst, tar.lzma, tar.Z) | Auto-detect |
| `-proxy` | HTTP proxy URL (e.g., http://proxy:8080) | None |
| `-limit` | Bandwidth limit (e.g., 1M, 500K, 2.5M) | Unlimited |
| `-memory` | Memory limit for buffers (e.g., 1G, 500M, auto) | auto (10% of system memory) |
| `-verbose` | Show detailed chunk-level progress | false |
| `-max-retries` | Maximum retry attempts for failed connections | 10 |
| `-retry-delay` | Initial retry delay (e.g., 1s, 500ms) | 1s |
| `-version` | Show version and exit | N/A |

## 🗂️ Supported Archive Formats

The tool automatically detects and extracts the following formats:

- `.zip` - ZIP archives
- `.tar` - Uncompressed tar
- `.tar.gz`, `.tgz` - Gzip compressed
- `.tar.bz2`, `.tbz2`, `.tbz` - Bzip2 compressed
- `.tar.xz`, `.txz` - XZ compressed
- `.tar.lz4` - LZ4 compressed
- `.tar.zst`, `.tar.zstd` - Zstandard compressed
- `.tar.lzma` - LZMA compressed
- `.tar.Z` - Compress (.Z) format

### Multi-Part Archives

You can download archives split across multiple URLs by providing all URLs as arguments:

```bash
./marianne https://cdn.example.com/data.tar.gz.part1 \
           https://cdn.example.com/data.tar.gz.part2 \
           https://cdn.example.com/data.tar.gz.part3
```

**How it works:**
- Each part is downloaded sequentially (one at a time)
- Each part uses parallel chunk downloading for maximum speed
- For TAR archives: parts are streamed and concatenated directly to extraction (no temp files)
- For ZIP archives: parts are concatenated to a temp file, then extracted
- Pre-flight validation checks all URLs before downloading
- Progress shows both overall progress and current part

**Use case:** Large blockchain snapshots, datasets, or backups that are distributed as multi-part archives.

## 🎮 Keyboard Controls

- `q` or `Ctrl+C` - Quit the application

## 🏗️ Building

```bash
# Standard build
make build

# Static build (no external dependencies)
make static

# Clean build artifacts
make clean
```

## 🚀 Performance Tips

1. **Increase workers** for better speeds on fast connections:
   ```bash
   ./marianne -workers 16 URL
   ```

2. **Adjust memory limit** based on your system:
   - Systems with limited RAM: Use smaller memory limits (e.g., `-memory 500M`)
   - High-memory systems: Increase limit for better buffering (e.g., `-memory 4G`)
   - The tool automatically calculates optimal chunk sizes based on memory

3. **Adjust chunk size** based on your connection:
   - Slower connections: Use smaller chunks (1-2MB)
   - Faster connections: Use larger chunks (10MB+)
   - Default is 2MB, automatically adjusted based on memory settings

4. **Use SSD** for extraction target to avoid I/O bottlenecks

5. **Configure retries** for unreliable connections:
   ```bash
   ./marianne -max-retries 20 -retry-delay 2s URL
   ```

## 🤝 Contributing

Contributions are welcome! Please feel free to submit a Pull Request. For major changes, please open an issue first to discuss what you would like to change.

## 📝 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## 🙏 Acknowledgments

- Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea) for the beautiful TUI
- Inspired by the need for faster downloads of large blockchain snapshots

## 📊 Benchmarks

Example downloading a 5GB file on a gigabit connection:

| Tool | Time | Speed |
|------|------|-------|
| curl | 8m 32s | ~10 MB/s |
| **marianne** | **2m 15s** | **~38 MB/s** |

*Results may vary based on server capabilities and network conditions*

## ⚙️ Advanced Features

### Memory Management
Marianne automatically manages memory usage to prevent system overload:
- Default: Uses 10% of system memory for buffering
- Adjusts chunk sizes based on available memory
- Prevents excessive memory consumption on large parallel downloads
- Configure manually with `-memory` flag for fine-tuning

### Retry Logic
Robust retry mechanism with exponential backoff:
- Automatically retries failed chunk downloads (default: 10 attempts)
- Exponential backoff prevents server overload (1s to 30s delay)
- Configurable with `-max-retries` and `-retry-delay` flags
- Individual chunk retries don't affect other parallel downloads

### Chunk-Level Progress
With `-verbose` flag, monitor individual chunk progress:
- Real-time status of each downloading chunk
- Per-chunk download speeds
- Worker assignment and timing information
- Useful for debugging and performance optimization

### Security Features
Built-in security protections for safe archive extraction:
- **Path Traversal Prevention**: Validates all ZIP entry paths
- **Absolute Path Blocking**: Rejects absolute paths in archives
- **Directory Escape Detection**: Ensures extracted files stay within output directory
- **Symlink Safety**: Proper handling of file permissions and modes

## 🐛 Known Issues

- RAR and 7z formats are not yet supported
- Windows support requires tar to be installed (available in Windows 10+)
- Downloads cannot be resumed after interruption (future feature)

## 🗺️ Roadmap

- [x] Support for ZIP archives
- [x] HTTP proxy support
- [x] Bandwidth limiting options
- [x] Memory management and optimization
- [x] Retry logic with exponential backoff
- [x] Verbose mode for chunk-level debugging
- [x] Multi-link archive support (split archives)
- [x] Archive format override flag
- [ ] Resume interrupted downloads
- [ ] Configuration file support
- [ ] Parallel ZIP extraction
- [ ] Support for RAR and 7z archives
- [ ] Streaming mode (extract without full download for TAR)