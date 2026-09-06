package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/benjaco/devflow/pkg/instance"
)

func observeLogEvidence(ctx context.Context, root, instanceID, runID string) (context.Context, context.CancelFunc, error) {
	path, err := instance.RunPath(root, instanceID, runID)
	if err != nil {
		return nil, nil, err
	}
	observed, cancel := context.WithCancelCause(ctx)
	check := func() bool {
		_, err := os.Stat(path)
		if err == nil {
			return false
		}
		if os.IsNotExist(err) {
			err = fmt.Errorf("run %s: %w", runID, instance.ErrRunExpired)
		}
		cancel(err)
		return true
	}
	// An attempt pathname never rotates. A retired run cannot produce more logs.
	if !check() {
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-observed.Done():
					return
				case <-ticker.C:
					if check() {
						return
					}
				}
			}
		}()
	}
	return observed, func() { cancel(context.Canceled) }, nil
}

func retainedLogError(ctx context.Context, root, instanceID, runID string, err error) error {
	if runID == "" || err == nil {
		return err
	}
	if errors.Is(err, context.Canceled) && context.Cause(ctx) != nil {
		return runEvidenceError(context.Cause(ctx))
	}
	if os.IsNotExist(err) {
		if _, loadErr := instance.LoadRun(root, instanceID, runID); errors.Is(loadErr, instance.ErrRunExpired) {
			return runEvidenceError(fmt.Errorf("run %s: %w", runID, loadErr))
		}
	}
	return err
}
