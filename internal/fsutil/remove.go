package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
)

// RemoveAllWritable repairs restrictive copied/cache permissions before
// cleanup. WalkDir does not follow symlinks, so chmod never escapes path.
func RemoveAllWritable(path string) error {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := makeWritable(path); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func makeWritable(path string) error {
	apply := func(candidate string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		mode := info.Mode().Perm() | 0o600
		if info.IsDir() {
			mode |= 0o100
		}
		return os.Chmod(candidate, mode)
	}
	return filepath.WalkDir(path, apply)
}
