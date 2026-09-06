package lock

import (
	"context"
	"errors"
	"time"
)

// AcquireContext keeps waits interruptible; a blocking OS lock cannot observe a
// canceled CLI context until the other builder finishes.
func AcquireContext(ctx context.Context, path string) (*FileLock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := TryAcquire(path)
		if !errors.Is(err, ErrLocked) {
			return file, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
