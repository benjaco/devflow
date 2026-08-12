//go:build windows

package fsutil

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const (
	windowsReplaceRetryLimit = 2 * time.Second
	windowsReplaceMaxDelay   = 50 * time.Millisecond
)

func replaceFile(oldPath, newPath string) error {
	deadline := time.Now().Add(windowsReplaceRetryLimit)
	delay := time.Millisecond
	for {
		err := os.Rename(oldPath, newPath)
		if err == nil || !transientWindowsReplaceError(err) {
			return err
		}
		if time.Now().Add(delay).After(deadline) {
			return err
		}
		time.Sleep(delay)
		delay = min(delay*2, windowsReplaceMaxDelay)
	}
}

func transientWindowsReplaceError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
