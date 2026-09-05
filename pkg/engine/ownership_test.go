package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/execution"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

type ownershipProject struct {
	configure func() error
	run       project.RunFunc
}

func (p ownershipProject) Name() string { return "ownership" }
func (p ownershipProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	if p.configure != nil {
		if err := p.configure(); err != nil {
			return project.InstanceConfig{}, err
		}
	}
	return project.InstanceConfig{Label: "ownership", Env: map[string]string{"VALUE": "owner"}}, nil
}
func (p ownershipProject) Tasks() []project.Task {
	return []project.Task{{Name: "check", Kind: project.KindOnce, Run: p.run}}
}
func (p ownershipProject) Targets() []project.Target {
	return []project.Target{{Name: "verify", RootTasks: []string{"check"}}}
}

func TestExecutionOwnershipRejectsBeforeMutation(t *testing.T) {
	for _, modes := range [][2]api.RunMode{{api.ModeCI, api.ModeCI}, {api.ModeWatch, api.ModeCI}, {api.ModeCI, api.ModeWatch}} {
		t.Run(string(modes[0])+"_"+string(modes[1]), func(t *testing.T) {
			root := t.TempDir()
			entered := make(chan struct{})
			release := make(chan struct{})
			owner, err := New(ownershipProject{run: func(ctx context.Context, rt *project.Runtime) error {
				if err := os.WriteFile(rt.Abs("artifact.txt"), []byte("owner"), 0o600); err != nil {
					return err
				}
				if err := os.WriteFile(instance.LogPath(root, rt.Instance.ID, "check"), []byte("owner log\n"), 0o600); err != nil {
					return err
				}
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}}, root)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				req := Request{Target: "verify", Worktree: root, Mode: modes[0]}
				if modes[0] == api.ModeWatch {
					done <- owner.Watch(ctx, req)
				} else {
					_, err := owner.Run(ctx, req)
					done <- err
				}
			}()
			t.Cleanup(func() {
				cancel()
				close(release)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("owner did not exit")
				}
			})
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("owner did not start")
			}
			id, _, _ := instance.IDForWorktree(root)
			paths := []string{filepath.Join(root, ".devflow", "state", "instances", id, "instance.json"), filepath.Join(root, ".devflow", "state", "instances", id, "runtime.env"), filepath.Join(root, ".devflow", "state", "instances", id, "status.json"), instance.LogPath(root, id, "check"), filepath.Join(root, "artifact.txt")}
			before := make(map[string]string)
			for _, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = string(data)
			}
			var configured, executed atomic.Bool
			contender, err := New(ownershipProject{configure: func() error { configured.Store(true); return nil }, run: func(_ context.Context, rt *project.Runtime) error {
				executed.Store(true)
				return os.WriteFile(rt.Abs("artifact.txt"), []byte("contender"), 0o600)
			}}, root)
			if err != nil {
				t.Fatal(err)
			}
			attemptCtx, stop := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer stop()
			req := Request{Target: "verify", Worktree: root, Mode: modes[1]}
			if modes[1] == api.ModeWatch {
				err = contender.Watch(attemptCtx, req)
			} else {
				_, err = contender.Run(attemptCtx, req)
			}
			var conflict *execution.ConflictError
			if !errors.As(err, &conflict) || conflict.Code() != "resource_conflict" {
				t.Errorf("competing execution error = %v; want resource_conflict", err)
			}
			if configured.Load() || executed.Load() {
				t.Errorf("rejected contender invoked callbacks: configured=%t executed=%t", configured.Load(), executed.Load())
			}
			for _, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != before[path] {
					t.Errorf("contender changed owner file %s", filepath.Base(path))
				}
			}
		})
	}
}

type failedStopHandle struct{ calls atomic.Int32 }

func (h *failedStopHandle) Alive() bool { return true }
func (h *failedStopHandle) PID() int    { return 0 }
func (h *failedStopHandle) Wait() error { return nil }
func (h *failedStopHandle) Stop() error { h.calls.Add(1); return errors.New("resource still running") }

type interruptedWatchStopHandle struct {
	*genericServiceHandle
	calls atomic.Int32
	err   error
}

func (h *interruptedWatchStopHandle) Stop() error {
	if h.calls.Add(1) == 1 {
		return errors.Join(context.Canceled, h.err)
	}
	return h.genericServiceHandle.Stop()
}

func TestWatchPreservesInterruptedRestartFailure(t *testing.T) {
	root := t.TempDir()
	writeWatchFreshnessInput(t, root, "old")
	stopErr := errors.New("restart stop failed")
	handle := &interruptedWatchStopHandle{genericServiceHandle: newGenericServiceHandle(), err: stopErr}
	p := watchPolicyFreshnessProject{tasks: []project.Task{{
		Name: "serve", Kind: project.KindService,
		Inputs: project.Inputs{Files: []string{"input.txt"}},
		Run: func(_ context.Context, rt *project.Runtime) error {
			rt.RegisterServiceHandle(handle)
			return nil
		},
	}}}
	eng, err := New(p, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var watchErr error
	go func() {
		defer close(done)
		watchErr = eng.Watch(ctx, Request{Worktree: root, Target: "dev", Mode: api.ModeWatch})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("watch did not exit")
		}
	})
	id := waitForEngineWatchReady(t, root)
	writeWatchFreshnessInput(t, root, "changed input")
	writeEngineFlushRequest(t, root, id)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not report its restart failure")
	}
	if !errors.Is(watchErr, stopErr) {
		t.Errorf("watch error = %v; want restart failure even when final cleanup succeeds", watchErr)
	}
	if handle.Alive() {
		t.Error("final cleanup did not stop the service")
	}
}

func TestExecutionOwnershipCleanupFailureDoesNotPermitReplacement(t *testing.T) {
	root := t.TempDir()
	handle := &failedStopHandle{}
	eng, err := New(ownershipProject{run: func(_ context.Context, rt *project.Runtime) error { rt.RegisterServiceHandle(handle); return nil }}, root)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := eng.Run(context.Background(), Request{Target: "verify", Worktree: root, Mode: api.ModeCI})
	if err == nil || outcome == nil || outcome.Result.Success {
		t.Errorf("cleanup failure reported success: outcome=%+v err=%v", outcome, err)
	}
	var ran atomic.Bool
	next, err := New(ownershipProject{run: func(context.Context, *project.Runtime) error { ran.Store(true); return nil }}, root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = next.Run(context.Background(), Request{Target: "verify", Worktree: root, Mode: api.ModeCI})
	if err == nil || ran.Load() {
		t.Errorf("replacement admitted after failed cleanup: ran=%t err=%v", ran.Load(), err)
	}
}

func TestExecutionOwnershipWatchScanFailureCleansResources(t *testing.T) {
	root := t.TempDir()
	handle := newGenericServiceHandle()
	eng, err := New(ownershipProject{run: func(_ context.Context, rt *project.Runtime) error {
		rt.RegisterServiceHandle(handle)
		path := filepath.Dir(instance.FlushSyncDir(root, rt.Instance.ID))
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("not a directory"), 0o600)
	}}, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := eng.Watch(ctx, Request{Target: "verify", Worktree: root, Mode: api.ModeWatch}); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expected watcher scan error")
	}
	if !handle.stopped.Load() {
		t.Fatal("watcher scan error left registered resource running")
	}
}
