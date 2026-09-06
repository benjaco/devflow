package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

type cancellationCLIHandle struct{ stopped atomic.Bool }

func (*cancellationCLIHandle) PID() int      { return 0 }
func (h *cancellationCLIHandle) Alive() bool { return !h.stopped.Load() }
func (*cancellationCLIHandle) Wait() error   { return nil }
func (h *cancellationCLIHandle) Stop() error { h.stopped.Store(true); return nil }

type cancellationCLIProject struct {
	started chan struct{}
	handle  *cancellationCLIHandle
}

func (*cancellationCLIProject) Name() string { return "cli-cancellation-project" }
func (*cancellationCLIProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{}, nil
}
func (*cancellationCLIProject) Targets() []project.Target {
	return []project.Target{{Name: "verify", RootTasks: []string{"wait"}}}
}
func (p *cancellationCLIProject) Tasks() []project.Task {
	return []project.Task{{
		Name: "wait", Kind: project.KindOnce,
		Run: func(ctx context.Context, rt *project.Runtime) error {
			rt.RegisterServiceHandle(p.handle)
			if err := os.WriteFile(rt.Abs("frontend/app.txt"), []byte("unfinished repair\n"), 0o644); err != nil {
				return err
			}
			close(p.started)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return errors.New("task context did not receive CLI cancellation")
			}
		},
	}}
}

func TestRunCancellationCleansHandlesAndSkipsRepositoryCommit(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	head := repairGitText(t, worktree, "rev-parse", "HEAD")
	p := &cancellationCLIProject{started: make(chan struct{}), handle: &cancellationCLIHandle{}}
	project.Register(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	app := &App{Context: ctx, Stdout: &stdout, Stderr: &stderr}
	finished := make(chan error, 1)
	go func() {
		finished <- app.Run([]string{
			"run", "verify", "--ci", "--json", "--project", p.Name(), "--worktree", worktree,
			"--commit-changes", "--commit-path", "frontend", "--commit-message", "must not commit interrupted work", "--push",
		})
	}()
	select {
	case <-p.started:
	case err := <-finished:
		t.Fatalf("run ended before task started: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not reach cancellable task")
	}
	cancel()
	err := <-finished
	if !errors.Is(err, context.Canceled) {
		t.Errorf("run cancellation = %v, want context.Canceled", err)
	}
	var result api.RunResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("expected one cancellation result: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	if result.Success || result.Error == nil || result.Error.Code != "operation_cancelled" || result.Error.Phase != "execution" {
		t.Errorf("unexpected cancellation result: %+v", result)
	}
	if p.handle.Alive() {
		t.Error("canceled CLI left its registered handle alive")
	}
	repair := result.RepositoryChanges
	if repair == nil || repair.Status != api.RepositoryChangeStatusSkippedDAGFailed || repair.CommitSHA != "" || repair.PushAttempted {
		t.Errorf("canceled DAG attempted repository finalization: %+v", repair)
	}
	if got := repairGitText(t, worktree, "rev-parse", "HEAD"); got != head {
		t.Errorf("canceled DAG changed HEAD from %s to %s", head, got)
	}
	if got := repairGitText(t, worktree, "diff", "--cached", "--name-only"); got != "" {
		t.Errorf("canceled DAG left staged changes: %q", got)
	}
}
