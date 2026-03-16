//go:build !windows

package main

import "syscall"

// getAvailableDiskSpace returns available disk space in bytes for the given path
func getAvailableDiskSpace(path string) (int64, error) {
	var stat syscall.Statfs_t
	err := syscall.Statfs(path, &stat)
	if err != nil {
		return 0, err
	}
	// Available blocks * block size = available space
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
