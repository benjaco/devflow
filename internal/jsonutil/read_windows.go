//go:build windows

package jsonutil

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const (
	windowsReadRetryLimit = 2 * time.Second
	windowsReadMaxDelay   = 50 * time.Millisecond
)

func readFile(path string) ([]byte, error) {
	deadline := time.Now().Add(windowsReadRetryLimit)
	delay := time.Millisecond
	for {
		data, err := os.ReadFile(path)
		if err == nil || !transientWindowsReadError(err) {
			return data, err
		}
		// MoveFileEx can hold a destination handle briefly while an atomic
		// state replacement commits. Readers use the same bounded backoff as
		// writers so a daemon request does not fail on that transient lock.
		if time.Now().Add(delay).After(deadline) {
			return nil, err
		}
		time.Sleep(delay)
		delay = min(delay*2, windowsReadMaxDelay)
	}
}

func transientWindowsReadError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
