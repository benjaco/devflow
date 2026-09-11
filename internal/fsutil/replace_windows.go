//go:build windows

package fsutil

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func replaceFile(oldPath, newPath string) error {
	err := renameWithRetry(context.Background(), oldPath, newPath, os.Rename, transientRenameError)
	var native *os.LinkError
	if errors.As(err, &native) {
		// State-file callers use os.Is* helpers, which expect native path errors.
		return native
	}
	return err
}

func transientRenameError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
