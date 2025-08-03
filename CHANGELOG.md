# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.0.0] - 2024-01-XX

### Added
- Initial release
- Parallel downloading with configurable workers
- Beautiful TUI with progress bar and file extraction view
- Automatic archive type detection
- Support for multiple compression formats (gz, bz2, xz, lz4, zst, lzma)
- Output directory option with automatic creation
- Memory-efficient streaming for large files
- Cross-platform support (Linux, macOS, Windows)
- Static binary builds via GitHub Actions

### Features
- Real-time download statistics (speed, ETA, progress)
- Graceful shutdown with Ctrl+C
- Optimized chunk sizes for maximum performance
- Concurrent chunk downloading with in-order assembly

[Unreleased]: https://github.com/wesraph/marianne/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/wesraph/marianne/releases/tag/v1.0.0