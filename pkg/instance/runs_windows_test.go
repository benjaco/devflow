//go:build windows

package instance

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

func TestRunPruneWithOpenWindowsAttemptKeepsListingHealthy(t *testing.T) {
	worktree := t.TempDir()
	active := createStoredRun(t, worktree, api.RunRunning, time.Time{})
	completed := createStoredRun(t, worktree, api.RunSucceeded, time.Now().Add(-2*time.Hour))
	path := mustAttemptLogPath(t, worktree, completed.InstanceID, completed.RunID, NewAttemptID())
	if err := os.WriteFile(path, []byte("held historical log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// os.Open does not grant FILE_SHARE_DELETE. Keep the handle open just as
	// a log follower does while its output consumer has stopped reading.
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	_, pruneErr := PruneRuns(worktree, completed.InstanceID, RunRetention{MaxAge: time.Hour})
	records, err := ListRuns(worktree, completed.InstanceID)
	if err != nil {
		t.Fatalf("open Windows log poisoned run listing: %v (prune: %v)", err, pruneErr)
	}
	retained, err := LoadRun(worktree, completed.InstanceID, completed.RunID)
	switch {
	case err == nil:
		// Some filesystems refuse to rename a directory containing an open
		// file. That must leave the original complete record published.
		if pruneErr == nil || retained.State != api.RunSucceeded || len(records) != 2 {
			t.Fatalf("blocked retirement lost evidence: record=%+v records=%d prune=%v", retained, len(records), pruneErr)
		}
		if bytes, err := os.ReadFile(path); err != nil || string(bytes) != "held historical log\n" {
			t.Fatalf("blocked retirement changed attempt log: %q %v", bytes, err)
		}
	case errors.Is(err, ErrRunExpired):
		if len(records) != 1 || records[0].RunID != active.RunID {
			t.Fatalf("retired run remained visible: %+v", records)
		}
	default:
		t.Fatalf("run is neither complete nor explicitly expired: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneRuns(worktree, completed.InstanceID, RunRetention{MaxAge: time.Hour}); err != nil {
		t.Fatalf("cleanup did not recover after reader closed: %v", err)
	}
	if _, err := LoadRun(worktree, completed.InstanceID, completed.RunID); !errors.Is(err, ErrRunExpired) {
		t.Fatalf("completed run not expired after retry: %v", err)
	}
}
