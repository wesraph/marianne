<div align="center">
  <img src="logo.svg" alt="Marianne Logo" width="200" height="200">
  
  # 🚀 Marianne

  A blazing-fast parallel downloader with automatic archive extraction and a beautiful terminal UI. Download large files at maximum speed using concurrent connections and extract them on-the-fly.
</div>

<div align="center">

![Go Version](https://img.shields.io/badge/Go-1.21%2B-blue)
![License](https://img.shields.io/badge/license-MIT-green)
![Platform](https://img.shields.io/badge/platform-linux%20%7C%20macos%20%7C%20windows-lightgrey)
[![CI](https://github.com/wesraph/marianne/actions/workflows/ci.yml/badge.svg)](https://github.com/wesraph/marianne/actions/workflows/ci.yml)
[![Release](https://github.com/wesraph/marianne/actions/workflows/release.yml/badge.svg)](https://github.com/wesraph/marianne/actions/workflows/release.yml)

</div>

## ✨ Features

- **⚡ Parallel Downloads**: Split files into chunks and download them concurrently for maximum speed
- **📊 Beautiful TUI**: Real-time progress bar, download stats, and live file extraction view
- **🗜️ Auto-extraction**: Automatically detects and extracts various archive formats
- **💾 Memory Efficient**: Streams data directly to tar, perfect for terabyte-sized files
- **📁 Output Control**: Specify output directory with automatic creation
- **🎯 Smart Chunking**: Optimized chunk sizes for best performance

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

- Go 1.21 or higher
- `tar` command (pre-installed on most Unix systems)
- For `.tar.lz4`: `lz4` command
- For `.tar.zst`: `zstd` command

## 📖 Usage

### Basic Usage

```bash
./marianne https://example.com/large-file.tar.gz
```

### With Options

```bash
# Download to specific directory
./marianne -output /path/to/extract https://example.com/archive.tar.xz

# Customize workers and chunk size
./marianne -workers 16 -chunk 209715200 https://example.com/huge.tar.lz4
```

### Options

| Flag | Description | Default |
|------|-------------|---------|
| `-workers` | Number of parallel download workers | 8 |
| `-chunk` | Chunk size in bytes | 104857600 (100MB) |
| `-output` | Output directory (creates if doesn't exist) | Current directory |

## 🗂️ Supported Archive Formats

The tool automatically detects and extracts the following formats:

- `.tar` - Uncompressed tar
- `.tar.gz`, `.tgz` - Gzip compressed
- `.tar.bz2`, `.tbz2`, `.tbz` - Bzip2 compressed
- `.tar.xz`, `.txz` - XZ compressed
- `.tar.lz4` - LZ4 compressed
- `.tar.zst`, `.tar.zstd` - Zstandard compressed
- `.tar.lzma` - LZMA compressed
- `.tar.Z` - Compress (.Z) format

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

2. **Adjust chunk size** based on your connection:
   - Slower connections: Use smaller chunks (50MB)
   - Faster connections: Use larger chunks (200MB+)

3. **Use SSD** for extraction target to avoid I/O bottlenecks

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

## 🐛 Known Issues

- ZIP, RAR, and 7z formats are not yet supported
- Windows support requires tar to be installed (available in Windows 10+)

## 🗺️ Roadmap

- [ ] Support for ZIP archives
- [ ] Resume interrupted downloads
- [ ] Configuration file support
- [ ] Bandwidth limiting options
- [ ] HTTP proxy support