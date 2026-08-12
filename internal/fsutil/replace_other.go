//go:build !windows

package fsutil

import "os"

func replaceFile(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}
