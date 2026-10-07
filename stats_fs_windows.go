//go:build windows

package storage

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// getFilesystemStatistics returns the capacity of the filesystem containing
// path using GetDiskFreeSpaceEx.  Inode accounting is not a meaningful
// concept on Windows and is therefore left at zero.
func getFilesystemStatistics(path string) (FilesystemStatistics, error) {
	pathPtr, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return FilesystemStatistics{}, err
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		return FilesystemStatistics{}, err
	}
	return FilesystemStatistics{
		TotalBytes:     int64(totalBytes),
		FreeBytes:      int64(totalFreeBytes),
		AvailableBytes: int64(freeBytesAvailable),
	}, nil
}

// syncDirectory is a no-op on Windows: renames of closed files are already
// durable enough for the generation protocol on this platform.
func syncDirectory(path string) error {
	return nil
}
