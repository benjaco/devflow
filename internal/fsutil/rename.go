package fsutil

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

const (
	renameRetryLimit = 2 * time.Second
	renameMaxDelay   = 50 * time.Millisecond
)

func renameWithRetry(ctx context.Context, source, destination string, rename func(string, string) error, retryable func(error) bool) error {
	started := time.Now()
	var lastError error
	retries := 0
	transient := false
	finish := func() error {
		detail := ""
		var errno syscall.Errno
		if errors.As(lastError, &errno) {
			detail = fmt.Sprintf("; native error %d", errno)
		}
		if transient {
			detail += "; path may be locked or inaccessible"
		}
		// Keep cancellation and the last native refusal available to callers.
		return fmt.Errorf("rename %q to %q failed after %d retries (%s%s): %w", source, destination, retries, time.Since(started).Round(time.Millisecond), detail, errors.Join(ctx.Err(), lastError))
	}
	delay := time.Millisecond
	for {
		if ctx.Err() != nil {
			return finish()
		}
		if lastError != nil {
			if time.Since(started) >= renameRetryLimit {
				return finish()
			}
			retries++
		}
		err := rename(source, destination)
		if err == nil {
			return nil
		}
		lastError = err
		transient = retryable(err)
		if !transient {
			return finish()
		}
		remaining := renameRetryLimit - time.Since(started)
		if remaining <= 0 {
			return finish()
		}
		timer := time.NewTimer(min(delay, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return finish()
		case <-timer.C:
		}
		delay = min(delay*2, renameMaxDelay)
	}
}
