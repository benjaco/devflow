package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

type cacheDeclarationProject struct {
	testProject
	task project.Task
}

func (p cacheDeclarationProject) Tasks() []project.Task { return []project.Task{p.task} }
func (p cacheDeclarationProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{p.task.Name}}}
}

func TestEngineRequiresCachedOutputsBeforeExecution(t *testing.T) {
	p := cacheDeclarationProject{task: project.Task{Name: "generate", Kind: project.KindOnce, Cache: true}}
	worktree := t.TempDir()
	if _, err := New(p, worktree); err == nil || !strings.Contains(err.Error(), "outputs") {
		t.Fatalf("cacheable task without outputs must be rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".devflow")); !os.IsNotExist(err) {
		t.Fatalf("invalid cached task caused worktree mutation: %v", err)
	}
	for _, outputs := range []project.Outputs{
		{Paths: []string{"dist"}},
		{Files: []string{"dist/app"}},
		{Dirs: []string{"dist"}},
	} {
		p.task.Outputs = outputs
		if _, err := New(p, worktree); err != nil {
			t.Fatalf("valid output declaration rejected: %v", err)
		}
	}
	p.task = project.Task{Name: "install", Kind: project.KindOnce, Stamp: true}
	if _, err := New(p, worktree); err != nil {
		t.Fatalf("local install stamps may omit outputs: %v", err)
	}
}

func TestRunDoesNotExecuteAfterCacheRestoreOperationalFailure(t *testing.T) {
	isolateEngineUserCache(t)
	const secret = "cache-restore-secret"
	t.Setenv("DEVFLOW_TEST_CACHE_SECRET", secret)
	worktree := filepath.Join(t.TempDir(), secret)
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	runs := 0
	p := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("cache-restore-failure")
		build := b.Task("build").Run(func(_ context.Context, rt *project.Runtime) error {
			runs++
			if err := os.MkdirAll(rt.Abs("dist"), 0o755); err != nil {
				return err
			}
			return os.WriteFile(rt.Abs("dist/result.txt"), []byte("built artifact"), 0o600)
		}).OutputFiles("dist/result.txt")
		b.Target("build", build)
		return nil
	})
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Target: "build", Worktree: worktree, Mode: api.ModeCI}
	if _, err := eng.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(worktree, "dist")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "dist"), []byte("preserve this file"), 0o600); err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEvents()
	out, err := eng.Run(context.Background(), req)
	if err == nil || out == nil || out.Result.Success || out.Result.FailedNode != "build" {
		t.Fatalf("operational restore error was not a failed task: outcome=%+v error=%v", out, err)
	}
	if runs != 1 {
		t.Fatalf("task executed after an unsafe cache restore: runs=%d", runs)
	}
	log, err := os.ReadFile(out.Result.FailedNodeLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "E: cache restore failed: ") {
		t.Fatalf("retained attempt log omitted the restore failure: %q", log)
	}
	if strings.Contains(string(log), secret) || !strings.Contains(string(log), "[REDACTED]") || len(log) > 4*1024+4 {
		t.Fatalf("restore diagnostic was not bounded and redacted: %q", log)
	}
	record, err := instance.LoadRun(worktree, out.Instance.ID, out.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Attempts) != 1 || len(out.Result.Nodes) != 1 {
		t.Fatalf("unexpected restore evidence: attempts=%+v nodes=%+v", record.Attempts, out.Result.Nodes)
	}
	attempt, node := record.Attempts[0], out.Result.Nodes[0]
	if record.RunID == "" || node.RunID != record.RunID || attempt.AttemptID == "" || node.AttemptID != attempt.AttemptID || attempt.Task != "build" || attempt.LogPath != out.Result.FailedNodeLogPath {
		t.Fatalf("restore evidence lost its run/task/attempt identity: record=%+v node=%+v", record, node)
	}
	if attempt.Executed || attempt.State != api.StateFailed || !attempt.LogsComplete || attempt.LastError == "" || attempt.LastError != node.LastError {
		t.Fatalf("restore failure was misreported as callback execution: %+v", attempt)
	}
	diagnostics := 0
	for len(events) > 0 {
		event := <-events
		if event.Type != api.EventLogLine || !strings.HasPrefix(event.Line, "cache restore failed: ") {
			continue
		}
		diagnostics++
		if event.Stream != "stderr" || event.RunID != record.RunID || event.AttemptID != attempt.AttemptID || event.Task != "build" || strings.Contains(event.Line, secret) {
			t.Fatalf("restore event lost identity or leaked its secret: %+v", event)
		}
		if string(log) != "E: "+event.Line+"\n" {
			t.Fatalf("event and retained restore diagnostic disagree: event=%q log=%q", event.Line, log)
		}
	}
	if diagnostics != 1 {
		t.Fatalf("restore failure emitted %d diagnostics, want one", diagnostics)
	}
	contents, err := os.ReadFile(filepath.Join(worktree, "dist"))
	if err != nil || string(contents) != "preserve this file" {
		t.Fatalf("restore changed the obstructing user file: contents=%q error=%v", contents, err)
	}
}

func TestWatchRestoreFailureSurvivesCancellation(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	p := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("watch-restore-failure")
		build := b.Task("build").Run(func(_ context.Context, rt *project.Runtime) error {
			if err := os.MkdirAll(rt.Abs("dist"), 0o755); err != nil {
				return err
			}
			return os.WriteFile(rt.Abs("dist/result.txt"), []byte("cached artifact"), 0o600)
		}).OutputFiles("dist/result.txt")
		b.Target("build", build)
		return nil
	})
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background(), Request{Worktree: worktree, Target: "build", Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(worktree, "dist")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "dist"), []byte("preserve obstruction"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var watchErr error
	go func() {
		defer close(done)
		watchErr = eng.Watch(ctx, Request{Worktree: worktree, Target: "build", Mode: api.ModeWatch})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("watch did not finish cleanup")
		}
	})
	id := waitForEngineWatchReady(t, worktree)
	status, err := instance.LoadStatus(worktree, id)
	if err != nil {
		t.Fatal(err)
	}
	before, err := instance.LoadRun(worktree, id, status.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Attempts) != 1 || before.Attempts[0].State != api.StateFailed || before.Attempts[0].Executed || !before.Attempts[0].LogsComplete || !strings.Contains(before.Attempts[0].LastError, "not a directory") {
		t.Fatalf("watch did not retain its completed restore failure: %+v", before)
	}
	logBefore, err := os.ReadFile(before.Attempts[0].LogPath)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop after cancellation")
	}
	if watchErr != nil {
		t.Fatalf("watch cleanup failed: %v", watchErr)
	}
	after, err := instance.LoadRun(worktree, id, before.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != api.RunCanceled || !reflect.DeepEqual(before.Attempts, after.Attempts) {
		t.Fatalf("watch cancellation rewrote the failed attempt: before=%+v after=%+v", before, after)
	}
	logAfter, err := os.ReadFile(before.Attempts[0].LogPath)
	if err != nil || string(logBefore) != string(logAfter) {
		t.Fatalf("watch cancellation changed the retained diagnostic: before=%q after=%q err=%v", logBefore, logAfter, err)
	}
}
