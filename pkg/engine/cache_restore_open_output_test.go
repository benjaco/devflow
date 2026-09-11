package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func TestCacheRestoreWithOpenOutputRetainsEvidence(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := filepath.Join(t.TempDir(), "worktree with spaces")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	executions := 0
	p := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("open-output-cache-restore-evidence")
		generate := b.Task("generate").Run(func(_ context.Context, rt *project.Runtime) error {
			executions++
			if err := os.MkdirAll(rt.Abs("generated client"), 0o755); err != nil {
				return err
			}
			return os.WriteFile(rt.Abs("generated client/client.txt"), []byte("cached client"), 0o600)
		}).OutputDirs("generated client")
		b.Target("build", generate)
		return nil
	})
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Target: "build", Worktree: worktree, Mode: api.ModeCI}
	seed, err := eng.Run(context.Background(), req)
	if err != nil || seed == nil || !seed.Result.Success || executions != 1 {
		t.Fatalf("seed cached output: outcome=%+v executions=%d error=%v", seed, executions, err)
	}
	outputDir := filepath.Join(worktree, "generated client")
	outputFile := filepath.Join(outputDir, "client.txt")
	if err := os.WriteFile(outputFile, []byte("current client"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The reader stays open throughout restoration. Filesystems may permit
	// replacement or refuse it; either outcome must preserve data and evidence.
	reader, err := os.Open(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if reader != nil {
			if err := reader.Close(); err != nil {
				t.Error(err)
			}
		}
	})

	outcome, runErr := eng.Run(context.Background(), req)
	if outcome == nil || executions != 1 {
		t.Fatalf("restore must return evidence without generation: outcome=%+v executions=%d error=%v", outcome, executions, runErr)
	}
	record, err := instance.LoadRun(worktree, outcome.Instance.ID, outcome.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Attempts) != 1 {
		t.Fatalf("restore run evidence: %+v", record)
	}
	attempt := record.Attempts[0]
	if attempt.Task != "generate" || attempt.Executed || !attempt.LogsComplete || attempt.AttemptID == "" {
		t.Fatalf("restore attempt evidence: %+v", attempt)
	}
	retainedLog, err := os.ReadFile(attempt.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	var renameErr *os.LinkError
	if runErr == nil {
		if !outcome.Result.Success || record.State != api.RunSucceeded || attempt.State != api.StateCached || attempt.LastError != "" {
			t.Fatalf("successful restore must record cached success: result=%+v record=%+v", outcome.Result, record)
		}
		if len(outcome.Result.CacheHits) != 1 || outcome.Result.CacheHits[0] != "generate" {
			t.Fatalf("successful restore must report a cache hit: %+v", outcome.Result)
		}
		if data, err := os.ReadFile(outputFile); err != nil || string(data) != "cached client" {
			t.Fatalf("successful restore did not publish cached content: %q, %v", data, err)
		}
	} else {
		if outcome.Result.Success || outcome.Result.FailedNode != "generate" || record.State != api.RunFailed || attempt.State != api.StateFailed || len(outcome.Result.CacheHits) != 0 {
			t.Fatalf("refused restore must record failure without a cache hit: result=%+v record=%+v error=%v", outcome.Result, record, runErr)
		}
		if !errors.As(runErr, &renameErr) || renameErr.Op != "rename" || renameErr.Old != outputDir {
			t.Fatalf("restore failed outside the open output backup rename: %v", runErr)
		}
		if !strings.Contains(attempt.LastError, renameErr.Err.Error()) {
			t.Fatalf("attempt lost filesystem failure: %+v; cause=%v", attempt, renameErr)
		}
		if data, err := os.ReadFile(outputFile); err != nil || string(data) != "current client" {
			t.Fatalf("refused restore changed original: %q, %v", data, err)
		}
	}
	if data, err := io.ReadAll(reader); err != nil || string(data) != "current client" {
		t.Fatalf("restore changed the open reader's content: %q, %v", data, err)
	}
	t.Logf("open-reader restore: run=%s attempt=%s state=%s executed=%t error=%v retainedLog=%q",
		record.RunID, attempt.AttemptID, attempt.State, attempt.Executed, runErr, retainedLog)

	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	reader = nil
	unlocked, err := eng.Run(context.Background(), req)
	if err != nil || unlocked == nil || !unlocked.Result.Success || len(unlocked.Result.CacheHits) != 1 || unlocked.Result.CacheHits[0] != "generate" || executions != 1 {
		t.Fatalf("unlocked control must reuse intact cache without generation: outcome=%+v executions=%d error=%v", unlocked, executions, err)
	}
	restored, err := os.ReadFile(outputFile)
	if err != nil || string(restored) != "cached client" {
		t.Fatalf("unlocked control did not restore cached content: %q, %v", restored, err)
	}
	t.Logf("unlocked control: run=%s cacheHits=%v executions=%d content=%q", unlocked.Result.RunID, unlocked.Result.CacheHits, executions, restored)

	// If publication was refused, copy counters alone cannot explain it.
	// Require the observed cause, regardless of which OS returned the error.
	if renameErr != nil {
		log := string(retainedLog)
		if !strings.Contains(log, renameErr.Err.Error()) || (!strings.Contains(log, outputDir) && !strings.Contains(log, strconv.Quote(outputDir))) {
			t.Fatalf("retained task log omits cache-restore failure cause/path: cause=%v log=%q", renameErr, retainedLog)
		}
	}
}
