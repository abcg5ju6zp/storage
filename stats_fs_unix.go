//go:build linux || darwin || freebsd

package storage

import (
	"os"

	"golang.org/x/sys/unix"
)

// getFilesystemStatistics returns the capacity of the filesystem containing
// path using statfs(2).
func getFilesystemStatistics(path string) (FilesystemStatistics, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return FilesystemStatistics{}, err
	}
	bsize := int64(st.Bsize)
	if bsize == 0 {
		bsize = 1
	}
	return FilesystemStatistics{
		TotalBytes:      int64(st.Blocks) * bsize,
		FreeBytes:       int64(st.Bfree) * bsize,
		AvailableBytes:  int64(st.Bavail) * bsize,
		Inodes:          int64(st.Files),
		InodesAvailable: int64(st.Ffree),
	}, nil
}

// syncDirectory flushes directory entry changes (creation, rename, unlink) to
// stable storage.
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
