//go:build !(linux || darwin || freebsd || windows)

package storage

import "os"

// getFilesystemStatistics is a best-effort fallback on platforms for which no
// statfs wrapper is available.  Capacity statistics are not required for the
// consistency guarantees of statistics generations, so it reports no values
// instead of failing the refresh.
func getFilesystemStatistics(path string) (FilesystemStatistics, error) {
	return FilesystemStatistics{}, nil
}

// syncDirectory flushes directory entry changes (creation, rename, unlink) to
// stable storage where supported.
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
