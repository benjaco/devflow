package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/benjaco/devflow/internal/clierror"
)

// RequestRunCancellation addresses one retained run. It never falls back to the
// current instance owner, so an old command cannot stop a replacement watcher.
func RequestRunCancellation(ctx context.Context, worktree, instanceID, runID string) error {
	if _, err := RunPath(worktree, instanceID, runID); err != nil {
		return err
	}
	err := withRunStoreLock(ctx, worktree, instanceID, false, func() error {
		record, err := loadRunLocked(worktree, instanceID, runID)
		if err != nil {
			return err
		}
		if record.State.Terminal() {
			return clierror.Wrap(errors.New("run has already finished"), "run_not_active", "execution")
		}
		path, err := cancellationPath(worktree, instanceID, runID)
		if err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if os.IsExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return file.Close()
	})
	if os.IsNotExist(err) {
		return ErrRunUnknown
	}
	return err
}

// ObserveRunCancellation gives direct and daemon execution the same control
// path. The marker remains scoped to its run until that evidence is pruned.
func ObserveRunCancellation(ctx context.Context, worktree, instanceID, runID string) (context.Context, context.CancelFunc, error) {
	record, err := LoadRun(worktree, instanceID, runID)
	if err != nil {
		return nil, nil, err
	}
	if record.State.Terminal() {
		return nil, nil, clierror.Wrap(errors.New("run has already finished"), "run_not_active", "execution")
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	check := func() bool {
		err := CheckRunCancellation(ctx, worktree, instanceID, runID)
		if err == nil {
			return false
		}
		cancel(err)
		return true
	}
	if !check() {
		go func() {
			ticker := time.NewTicker(25 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-ticker.C:
					if check() {
						return
					}
				}
			}
		}()
	}
	return runCtx, func() { cancel(context.Canceled) }, nil
}

// CheckRunCancellation closes the interval between a queued request's last
// observer poll and admission to mutation of the current development instance.
func CheckRunCancellation(ctx context.Context, worktree, instanceID, runID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := cancellationPath(worktree, instanceID, runID)
	if err != nil {
		return err
	}
	_, err = os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return context.Canceled
}

func cancellationPath(worktree, instanceID, runID string) (string, error) {
	path, err := RunPath(worktree, instanceID, runID)
	if err != nil {
		return "", err
	}
	return filepath.Join(path, "cancel.request"), nil
}
