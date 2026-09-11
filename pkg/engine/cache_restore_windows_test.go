//go:build windows

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
	"golang.org/x/sys/windows"
)

func TestWindowsCacheRestoreFailureRetainsNativeCauseInTaskLog(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := filepath.Join(t.TempDir(), "worktree with spaces")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	executions := 0
	p := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("windows-cache-restore-evidence")
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
	path, err := windows.UTF16PtrFromString(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the child readable/writable but deny rename until the failed run
	// returns. This is a persistent-lock reproduction, not a timed retry test.
	handle, err := windows.CreateFile(path, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if handle != windows.InvalidHandle {
			if err := windows.CloseHandle(handle); err != nil {
				t.Error(err)
			}
		}
	})

	failed, runErr := eng.Run(context.Background(), req)
	if runErr == nil || failed == nil || failed.Result.Success || failed.Result.FailedNode != "generate" {
		t.Fatalf("locked restore must fail: outcome=%+v error=%v", failed, runErr)
	}
	var renameErr *os.LinkError
	if !errors.As(runErr, &renameErr) || renameErr.Op != "rename" || renameErr.Old != outputDir {
		t.Fatalf("failure must be the output backup rename: %v", runErr)
	}
	if !errors.Is(runErr, windows.ERROR_ACCESS_DENIED) && !errors.Is(runErr, windows.ERROR_SHARING_VIOLATION) && !errors.Is(runErr, windows.ERROR_LOCK_VIOLATION) {
		t.Fatalf("expected native Windows sharing/access conflict: %v", runErr)
	}
	if executions != 1 || len(failed.Result.CacheHits) != 0 {
		t.Fatalf("failed restore executed generator or reported a cache hit: executions=%d hits=%v", executions, failed.Result.CacheHits)
	}
	current, err := os.ReadFile(outputFile)
	if err != nil || string(current) != "current client" {
		t.Fatalf("failed restore changed original: %q, %v", current, err)
	}
	record, err := instance.LoadRun(worktree, failed.Instance.ID, failed.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != api.RunFailed || len(record.Attempts) != 1 {
		t.Fatalf("failed run evidence: %+v", record)
	}
	attempt := record.Attempts[0]
	if attempt.Task != "generate" || attempt.State != api.StateFailed || attempt.Executed || !attempt.LogsComplete || attempt.AttemptID == "" {
		t.Fatalf("failed restore attempt evidence: %+v", attempt)
	}
	if !strings.Contains(attempt.LastError, renameErr.Err.Error()) {
		t.Fatalf("attempt lost native failure: %+v; cause=%v", attempt, renameErr)
	}
	retainedLog, err := os.ReadFile(attempt.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("locked restore: run=%s attempt=%s executed=%t native=%v error=%v retainedLog=%q",
		record.RunID, attempt.AttemptID, attempt.Executed, renameErr.Err, runErr, retainedLog)

	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	handle = windows.InvalidHandle
	unlocked, err := eng.Run(context.Background(), req)
	if err != nil || unlocked == nil || !unlocked.Result.Success || len(unlocked.Result.CacheHits) != 1 || unlocked.Result.CacheHits[0] != "generate" || executions != 1 {
		t.Fatalf("unlocked control must reuse intact cache without generation: outcome=%+v executions=%d error=%v", unlocked, executions, err)
	}
	restored, err := os.ReadFile(outputFile)
	if err != nil || string(restored) != "cached client" {
		t.Fatalf("unlocked control did not restore cached content: %q, %v", restored, err)
	}
	t.Logf("unlocked control: run=%s cacheHits=%v executions=%d content=%q", unlocked.Result.RunID, unlocked.Result.CacheHits, executions, restored)

	// Copy counters do not explain why publication failed. Check this only
	// after both the real native failure and the unlocked control are verified.
	if !strings.Contains(string(retainedLog), renameErr.Err.Error()) || !strings.Contains(string(retainedLog), outputDir) {
		t.Fatalf("retained task log omits cache-restore failure cause/path: cause=%v log=%q", renameErr, retainedLog)
	}
}
