package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/jsonutil"
	"github.com/benjaco/devflow/pkg/api"
)

func createStoredRun(t *testing.T, worktree string, state api.RunState, finish time.Time) *api.RunRecord {
	t.Helper()
	record := &api.RunRecord{InstanceID: "test-instance", Project: "fixture", Target: "test", Mode: api.ModeCI, State: api.RunQueued}
	if err := CreateRun(worktree, record.InstanceID, record); err != nil {
		t.Fatal(err)
	}
	if record.RunID == "" {
		t.Fatal("run identity was not allocated before execution")
	}
	record.State = state
	record.FinishedAt = finish
	if err := SaveRun(worktree, record.InstanceID, record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestRunRecordsSurviveSubsequentRuns(t *testing.T) {
	worktree := t.TempDir()
	first := createStoredRun(t, worktree, api.RunRunning, time.Time{})
	first.Attempts = []api.TaskAttempt{{AttemptID: NewAttemptID(), Task: "test", State: api.StateFailed, Executed: true, CacheKey: "read-input-v1", LastError: "first failure"}}
	first.State = api.RunFailed
	first.FinishedAt = time.Now().UTC()
	first.Result = &api.RunResult{Target: "test", Success: false, FailedNode: "test"}
	if err := SaveRun(worktree, first.InstanceID, first); err != nil {
		t.Fatal(err)
	}
	second := createStoredRun(t, worktree, api.RunSucceeded, time.Now().UTC())
	if first.RunID == second.RunID {
		t.Fatal("later run reused an identity")
	}
	historical, err := LoadRun(worktree, first.InstanceID, first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if historical.Result == nil || historical.Result.Success || historical.Attempts[0].CacheKey != "read-input-v1" {
		t.Fatalf("history replaced: %+v", historical)
	}
	records, err := ListRuns(worktree, first.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].RunID != second.RunID {
		t.Fatalf("expected newest first, got %+v", records)
	}
	first.State = api.RunRunning
	if err := SaveRun(worktree, first.InstanceID, first); !errors.Is(err, ErrRunFinalized) {
		t.Fatalf("terminal record reopened: %v", err)
	}
}

func TestRunIDsAllocateConcurrentlyWithoutReuse(t *testing.T) {
	worktree := t.TempDir()
	const count = 24
	ids := make(chan string, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Go(func() {
			record := &api.RunRecord{InstanceID: "test-instance", Project: "fixture", Target: "test", State: api.RunQueued}
			if err := CreateRun(worktree, record.InstanceID, record); err != nil {
				errs <- err
				return
			}
			ids <- record.RunID
		})
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	seen := map[string]bool{}
	for id := range ids {
		if id == "" || seen[id] {
			t.Errorf("duplicate or empty ID %q", id)
		}
		seen[id] = true
	}
	if len(seen) != count {
		t.Fatalf("allocated %d distinct IDs, want %d", len(seen), count)
	}
}

func TestRunRetentionPreservesActiveAndClassifiesMissingIDs(t *testing.T) {
	worktree := t.TempDir()
	now := time.Now().UTC()
	active := createStoredRun(t, worktree, api.RunWaiting, time.Time{})
	old := createStoredRun(t, worktree, api.RunFailed, now.Add(-48*time.Hour))
	middle := createStoredRun(t, worktree, api.RunSucceeded, now.Add(-time.Hour))
	newest := createStoredRun(t, worktree, api.RunSucceeded, now)
	result, err := PruneRuns(worktree, active.InstanceID, RunRetention{MaxCompleted: 1, MaxAge: 24 * time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 2 || result.RetainedCompleted != 1 {
		t.Fatalf("wrong pruning: %+v", result)
	}
	for _, id := range []string{active.RunID, newest.RunID} {
		if _, err := LoadRun(worktree, active.InstanceID, id); err != nil {
			t.Errorf("retained %s: %v", id, err)
		}
	}
	for _, id := range []string{old.RunID, middle.RunID} {
		if _, err := LoadRun(worktree, active.InstanceID, id); !errors.Is(err, ErrRunExpired) {
			t.Errorf("expired %s: %v", id, err)
		}
	}
	if _, err := LoadRun(worktree, active.InstanceID, "run-test-instance-ffffffffffffffff"); !errors.Is(err, ErrRunUnknown) {
		t.Errorf("never issued ID: %v", err)
	}
	if _, err := LoadRun(worktree, active.InstanceID, "../../outside"); !errors.Is(err, ErrInvalidRunID) {
		t.Errorf("malformed ID: %v", err)
	}
	// A new allocation after all older evidence is deleted must still be unique.
	newer := createStoredRun(t, worktree, api.RunRunning, time.Time{})
	if newer.RunID <= newest.RunID {
		t.Fatalf("allocation restarted after pruning: %s <= %s", newer.RunID, newest.RunID)
	}
}

func TestRunRetentionBoundsCompletedBytesAndAttemptPaths(t *testing.T) {
	worktree := t.TempDir()
	now := time.Now().UTC()
	active := createStoredRun(t, worktree, api.RunRunning, time.Time{})
	completed := createStoredRun(t, worktree, api.RunSucceeded, now)
	firstID, secondID := NewAttemptID(), NewAttemptID()
	firstPath := mustAttemptLogPath(t, worktree, active.InstanceID, active.RunID, firstID)
	secondPath := mustAttemptLogPath(t, worktree, active.InstanceID, active.RunID, secondID)
	if firstPath == "" || firstPath == secondPath {
		t.Fatalf("attempt logs collide: %q, %q", firstPath, secondPath)
	}
	for _, path := range []string{firstPath, secondPath, mustAttemptLogPath(t, worktree, completed.InstanceID, completed.RunID, NewAttemptID())} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 2048)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := PruneRuns(worktree, active.InstanceID, RunRetention{MaxBytes: 1024, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != completed.RunID {
		t.Fatalf("byte budget did not expire completed evidence: %+v", result)
	}
	if data, err := os.ReadFile(firstPath); err != nil || len(data) != 2048 {
		t.Fatalf("active log removed: bytes=%d err=%v", len(data), err)
	}
	if path, err := AttemptLogPath(worktree, active.InstanceID, active.RunID, "../../outside"); err == nil || path != "" {
		t.Fatalf("invalid attempt escaped run: %q", path)
	}
}

func TestRunStoreRejectsCorruptionAndKeepsOwnerOnlyRecords(t *testing.T) {
	worktree := t.TempDir()
	record := createStoredRun(t, worktree, api.RunRunning, time.Time{})
	path := filepath.Join(mustRunPath(t, worktree, record.InstanceID, record.RunID), "record.json")
	if runtime.GOOS != "windows" {
		for _, p := range []string{path, filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
			st, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm()&0o077 != 0 {
				t.Errorf("evidence accessible to other users: %s mode=%v", p, st.Mode())
			}
		}
	}
	corrupt := []byte(`{"runId":`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRun(worktree, record.InstanceID, record.RunID); err == nil {
		t.Fatal("corrupt record silently accepted")
	}
	if err := SaveRun(worktree, record.InstanceID, record); err == nil {
		t.Fatal("corrupt record overwritten")
	}
	if _, err := ListRuns(worktree, record.InstanceID); err == nil {
		t.Fatal("corrupt record hidden from list")
	}
	if _, err := PruneRuns(worktree, record.InstanceID, RunRetention{MaxCompleted: 1}); err == nil {
		t.Fatal("retention discarded corruption")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(corrupt) {
		t.Fatalf("corrupt evidence changed: %q %v", data, err)
	}
}

func TestRunRecordReadersSeeCompleteSnapshots(t *testing.T) {
	worktree := t.TempDir()
	record := createStoredRun(t, worktree, api.RunRunning, time.Time{})
	path := filepath.Join(mustRunPath(t, worktree, record.InstanceID, record.RunID), "record.json")
	done := make(chan struct{})
	failures := make(chan error, 1)
	go func() {
		defer close(done)
		for range 40 {
			var snapshot api.RunRecord
			if err := jsonutil.ReadFile(path, &snapshot); err != nil {
				failures <- err
				return
			}
			if snapshot.RunID != record.RunID || snapshot.InstanceID != record.InstanceID {
				failures <- fmt.Errorf("incomplete snapshot: %+v", snapshot)
				return
			}
		}
	}()
	for range 20 {
		if err := SaveRun(worktree, record.InstanceID, record); err != nil {
			t.Fatal(err)
		}
	}
	<-done
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
}

func mustRunPath(t *testing.T, wt, id, runID string) string {
	t.Helper()
	p, err := RunPath(wt, id, runID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func mustAttemptLogPath(t *testing.T, wt, id, runID, attemptID string) string {
	t.Helper()
	p, err := AttemptLogPath(wt, id, runID, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunClaimRefusesParallelAndTerminalExecution(t *testing.T) {
	worktree := t.TempDir()
	record := &api.RunRecord{InstanceID: "test-instance", Project: "fixture", Target: "test", State: api.RunQueued}
	if err := CreateRun(worktree, record.InstanceID, record); err != nil {
		t.Fatal(err)
	}
	claimed, err := ClaimRun(worktree, record.InstanceID, record.RunID, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != api.RunRunning || claimed.OwnerPID != os.Getpid() || claimed.StartedAt.IsZero() {
		t.Fatalf("claim incomplete: %+v", claimed)
	}
	if _, err := ClaimRun(worktree, record.InstanceID, record.RunID, os.Getpid()); !errors.Is(err, ErrRunFinalized) {
		t.Fatalf("already claimed run executed twice: %v", err)
	}
	claimed.State = api.RunSucceeded
	claimed.FinishedAt = time.Now().UTC()
	if err := SaveRun(worktree, record.InstanceID, claimed); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimRun(worktree, record.InstanceID, record.RunID, os.Getpid()); !errors.Is(err, ErrRunFinalized) {
		t.Fatalf("terminal run executed again: %v", err)
	}
	if _, err := LoadRun(worktree, "other-instance", record.RunID); !errors.Is(err, ErrInvalidRunID) {
		t.Fatalf("cross-instance ID accepted: %v", err)
	}
}

func TestRunRetentionUsesConstantSizeExpiryLedger(t *testing.T) {
	worktree := t.TempDir()
	var first string
	for range 20 {
		record := createStoredRun(t, worktree, api.RunSucceeded, time.Now().UTC())
		if first == "" {
			first = record.RunID
		}
		if _, err := PruneRuns(worktree, record.InstanceID, RunRetention{MaxCompleted: 1}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(runsRoot(worktree, "test-instance"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected one retained run, index and lock, got %d entries", len(entries))
	}
	if _, err := LoadRun(worktree, "test-instance", first); !errors.Is(err, ErrRunExpired) {
		t.Fatalf("old ID lost expiry classification: %v", err)
	}
}

func TestRunStorePreservesMissingOrCorruptIdentityIndex(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%v", missing), func(t *testing.T) {
			worktree := t.TempDir()
			existing := createStoredRun(t, worktree, api.RunRunning, time.Time{})
			indexPath := runIndexPath(worktree, existing.InstanceID)
			if missing {
				if err := os.Remove(indexPath); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(indexPath, []byte(`{"issued":`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			next := &api.RunRecord{InstanceID: existing.InstanceID, Project: "fixture", Target: "test"}
			if err := CreateRun(worktree, existing.InstanceID, next); err == nil {
				t.Fatal("invalid index reset and run ID reused")
			}
			if retained, err := LoadRun(worktree, existing.InstanceID, existing.RunID); err != nil || retained.RunID != existing.RunID {
				t.Fatalf("existing evidence lost: %+v %v", retained, err)
			}
		})
	}
}

func TestRunTerminalEvidenceCannotBeRewritten(t *testing.T) {
	worktree := t.TempDir()
	record := createStoredRun(t, worktree, api.RunSucceeded, time.Now().UTC())
	path := filepath.Join(mustRunPath(t, worktree, record.InstanceID, record.RunID), "record.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	record.Result = &api.RunResult{Success: false, FailedNode: "fabricated-later-failure"}
	if err := SaveRun(worktree, record.InstanceID, record); !errors.Is(err, ErrRunFinalized) {
		t.Fatalf("terminal evidence was rewritten: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("terminal bytes changed after refused rewrite")
	}
}

func TestRunPrunePartialDeletionCannotPoisonPublishedEvidence(t *testing.T) {
	worktree := t.TempDir()
	active := createStoredRun(t, worktree, api.RunRunning, time.Time{})
	completed := createStoredRun(t, worktree, api.RunSucceeded, time.Now().Add(-2*time.Hour))
	path := mustRunPath(t, worktree, completed.InstanceID, completed.RunID)
	attemptPath := mustAttemptLogPath(t, worktree, completed.InstanceID, completed.RunID, NewAttemptID())
	if err := os.WriteFile(attemptPath, []byte("held log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := errors.New("injected open-file deletion failure")
	partialRemove := func(path string) error {
		if err := os.Remove(filepath.Join(path, "record.json")); err != nil {
			return err
		}
		return blocked
	}
	result, err := pruneRuns(worktree, completed.InstanceID, RunRetention{MaxAge: time.Hour}, os.Rename, partialRemove)
	if !errors.Is(err, blocked) {
		t.Fatalf("cleanup failure hidden: %v", err)
	}
	records, err := ListRuns(worktree, completed.InstanceID)
	if err != nil {
		t.Fatalf("partial cleanup poisoned run listing: %v", err)
	}
	if len(records) != 1 || records[0].RunID != active.RunID {
		t.Fatalf("retired record remained published: %+v", records)
	}
	if len(result.Removed) != 1 || result.Removed[0] != completed.RunID {
		t.Fatalf("committed retirement missing from partial result: %+v", result)
	}
	if _, err := LoadRun(worktree, completed.InstanceID, completed.RunID); !errors.Is(err, ErrRunExpired) {
		t.Fatalf("retired identity not expired: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("partly removed run directory still visible: %v", err)
	}
	// The next pruning pass must retry physical cleanup even with no eligible
	// completed records left in the visible namespace.
	if _, err := PruneRuns(worktree, completed.InstanceID, DefaultRunRetention); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(runsRoot(worktree, completed.InstanceID))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".prune-") {
			t.Fatalf("retired cleanup debris was not retried: %s", entry.Name())
		}
	}
}

func TestRunPruneRenameFailureRetainsCompleteEvidence(t *testing.T) {
	worktree := t.TempDir()
	record := createStoredRun(t, worktree, api.RunSucceeded, time.Now().Add(-2*time.Hour))
	path := mustRunPath(t, worktree, record.InstanceID, record.RunID)
	before, err := os.ReadFile(filepath.Join(path, "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("injected directory rename denied")
	rename := func(string, string) error { return denied }
	removed := false
	remove := func(string) error { removed = true; return nil }
	_, err = pruneRuns(worktree, record.InstanceID, RunRetention{MaxAge: time.Hour}, rename, remove)
	if !errors.Is(err, denied) {
		t.Fatalf("retirement failure hidden: %v", err)
	}
	if removed {
		t.Fatal("recursive deletion ran before successful retirement")
	}
	after, err := os.ReadFile(filepath.Join(path, "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("failed retirement changed published evidence")
	}
	if records, err := ListRuns(worktree, record.InstanceID); err != nil || len(records) != 1 {
		t.Fatalf("failed retirement corrupted listing: %+v %v", records, err)
	}
}

func TestRunPruneRetiredEvidencePreventsIdentityIndexReset(t *testing.T) {
	worktree := t.TempDir()
	record := createStoredRun(t, worktree, api.RunSucceeded, time.Now().Add(-2*time.Hour))
	blocked := errors.New("cleanup blocked")
	if _, err := pruneRuns(worktree, record.InstanceID, RunRetention{MaxAge: time.Hour}, os.Rename, func(string) error { return blocked }); !errors.Is(err, blocked) {
		t.Fatal(err)
	}
	if err := os.Remove(runIndexPath(worktree, record.InstanceID)); err != nil {
		t.Fatal(err)
	}
	next := &api.RunRecord{InstanceID: record.InstanceID, Project: "fixture", Target: "test"}
	if err := CreateRun(worktree, record.InstanceID, next); err == nil {
		t.Fatal("lost index reused identity while retired evidence remained")
	}
}
