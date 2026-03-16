//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// getAvailableDiskSpace returns available disk space in bytes for the given path
func getAvailableDiskSpace(path string) (int64, error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceEx := kernel32.NewProc("GetDiskFreeSpaceExW")

	var freeBytesAvailable int64
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}

	ret, _, err := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		0,
		0,
	)
	if ret == 0 {
		return 0, err
	}
	return freeBytesAvailable, nil
}
