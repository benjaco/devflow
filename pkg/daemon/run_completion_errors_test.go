package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

func TestDaemonCompletionErrorsInvalidateReturnedResult(t *testing.T) {
	for _, failure := range []string{"record_read", "final_save"} {
		t.Run(failure, func(t *testing.T) {
			worktree := t.TempDir()
			inst, err := instance.Resolve(worktree, "test")
			if err != nil {
				t.Fatal(err)
			}
			record := &api.RunRecord{InstanceID: inst.ID, Project: "fixture", Target: "check", Mode: api.ModeCI, OwnerPID: os.Getpid()}
			if err := instance.CreateRun(worktree, inst.ID, record); err != nil {
				t.Fatal(err)
			}
			record.State = api.RunRunning
			if err := instance.SaveRun(worktree, inst.ID, record); err != nil {
				t.Fatal(err)
			}
			runPath, err := instance.RunPath(worktree, inst.ID, record.RunID)
			if err != nil {
				t.Fatal(err)
			}
			recordPath := filepath.Join(runPath, "record.json")
			if failure == "record_read" {
				if err := os.WriteFile(recordPath, []byte("unreadable run JSON"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				makeRunRecordReadOnly(t, runPath, recordPath)
				// Reads and retention still succeed; only the final atomic record
				// replacement is denied by this filesystem fixture.
				if _, err := instance.PruneRuns(worktree, inst.ID, instance.DefaultRunRetention); err != nil {
					t.Fatalf("fixture prevented retention before final save: %v", err)
				}
			}
			result := &api.RunResult{RunID: record.RunID, InstanceID: inst.ID, Target: record.Target, Mode: record.Mode, Success: true}
			active := &activeRun{runID: record.RunID, result: result, cancel: func() {}, done: make(chan struct{}), operation: &runOperation{id: record.RunID, cancel: func() {}}}
			s := &Server{worktree: worktree, instanceID: inst.ID, active: active, subscribers: map[chan api.Event]bool{}}
			events := s.addSubscriber()
			defer s.removeSubscriber(events)
			s.finishActiveRun(active)
			<-active.done
			if active.err == nil {
				t.Fatal("expected evidence completion failure")
			}
			if result.Success || result.Error == nil || result.Error.Code != "evidence_write_failed" || result.Error.Phase != "execution" {
				t.Errorf("completed observer received contradictory success: result=%+v err=%v", result, active.err)
			}
			for len(events) > 0 {
				if event := <-events; event.Type == api.EventRunFinished {
					t.Errorf("published durable completion after evidence failure: %+v", event)
				}
			}
			if failure == "final_save" {
				retained, err := instance.LoadRun(worktree, inst.ID, record.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if retained.State != api.RunRunning || retained.Result != nil {
					t.Errorf("failed save changed retained evidence: %+v", retained)
				}
			}
		})
	}
}

func makeRunRecordReadOnly(t *testing.T, runPath, recordPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// Windows denies replacing a read-only destination; Unix permissions
		// instead control creation of the atomic temporary file in its directory.
		if err := os.Chmod(recordPath, 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(recordPath, 0o600) })
		if file, err := os.OpenFile(recordPath, os.O_WRONLY, 0); err == nil {
			_ = file.Close()
			t.Skip("filesystem does not enforce read-only file permissions")
		}
		return
	}
	if err := os.Chmod(runPath, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(runPath, 0o700) })
	if file, err := os.CreateTemp(runPath, "permission-probe-*"); err == nil {
		_ = file.Close()
		t.Skip("process bypasses directory write permissions")
	}
}
