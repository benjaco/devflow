package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/benjaco/devflow/internal/execution"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

// A nil Stop error is only an acknowledgment; Alive still determines whether
// this resource can conflict with a replacement execution.
type acknowledgedStopHandle struct {
	stopErr error
	calls   atomic.Int32
}

func (h *acknowledgedStopHandle) Alive() bool { return true }
func (h *acknowledgedStopHandle) PID() int    { return 0 }
func (h *acknowledgedStopHandle) Wait() error { return nil }
func (h *acknowledgedStopHandle) Stop() error { h.calls.Add(1); return h.stopErr }

type waitingAcknowledgedStopHandle struct {
	acknowledgedStopHandle
	waitDone <-chan struct{}
}

func (h *waitingAcknowledgedStopHandle) Wait() error { <-h.waitDone; return nil }

func TestWatchOwnershipTerminalEventIncludesCleanupFailure(t *testing.T) {
	worktree := t.TempDir()
	waitDone := make(chan struct{})
	defer close(waitDone)
	handle := &waitingAcknowledgedStopHandle{waitDone: waitDone}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng, err := New(ownershipProject{run: func(_ context.Context, rt *project.Runtime) error {
		rt.RegisterServiceHandle(handle)
		cancel()
		return nil
	}}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEvents()
	err = eng.Watch(ctx, Request{Target: "verify", Worktree: worktree, Mode: api.ModeWatch})
	if err == nil {
		t.Fatal("watch cancellation concealed failed cleanup")
	}
	finished := 0
	for _, event := range drainEvents(events) {
		if event.Type != api.EventRunFinished {
			continue
		}
		finished++
		if event.Success == nil || *event.Success || event.Error == "" {
			t.Errorf("watch published success before resource cleanup finished: %+v", event)
		}
	}
	if finished != 1 {
		t.Fatalf("terminal event count = %d, want one final result", finished)
	}
	contender, err := execution.Acquire(worktree, execution.Owner{Target: "replacement"})
	if contender != nil {
		_ = contender.Release()
		t.Fatal("failed watch cleanup permitted replacement")
	}
	var conflict *execution.ConflictError
	if !errors.As(err, &conflict) || !conflict.RecoveryRequired {
		t.Fatalf("failed watch cleanup lost recovery evidence: %v", err)
	}
}

func TestExecutionOwnershipChecksAliveAfterAcknowledgedStop(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		name := "engine_owned"
		if borrowed {
			name = "caller_owned"
		}
		t.Run(name, func(t *testing.T) {
			worktree := t.TempDir()
			ctx := context.Background()
			var lease *execution.Lease
			if borrowed {
				var err error
				lease, err = execution.Acquire(worktree, execution.Owner{Target: "verify", Mode: "ci", Kind: "cli"})
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Release()
				ctx = execution.ContextWithLease(ctx, lease)
			}
			handle := &acknowledgedStopHandle{}
			eng, err := New(ownershipProject{run: func(_ context.Context, rt *project.Runtime) error {
				rt.RegisterServiceHandle(handle)
				return nil
			}}, worktree)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := eng.Run(ctx, Request{Target: "verify", Worktree: worktree, Mode: api.ModeCI})
			if err == nil || outcome == nil || outcome.Result.Success {
				t.Fatalf("still-live resource reported successful cleanup: outcome=%+v err=%v", outcome, err)
			}
			status, err := instance.LoadStatus(worktree, outcome.Result.InstanceID)
			if err != nil {
				t.Fatal(err)
			}
			if status.Nodes["check"].State != api.StateDegraded {
				t.Fatalf("still-live resource lost degraded state: %+v", status.Nodes["check"])
			}
			if lease != nil {
				if lease.ValidFor(worktree) {
					t.Fatal("incomplete cleanup left borrowed lease reusable")
				}
				if err := lease.Release(); err != nil {
					t.Fatal(err)
				}
			}
			contender, err := execution.Acquire(worktree, execution.Owner{Target: "replacement"})
			if contender != nil {
				_ = contender.Release()
				t.Fatal("still-live resource permitted replacement")
			}
			var conflict *execution.ConflictError
			if !errors.As(err, &conflict) || !conflict.RecoveryRequired {
				t.Fatalf("cleanup did not retain recovery evidence: %v", err)
			}
		})
	}
}

func TestLifecycleRestartKeepsOwnerWhenStopCannotConfirmExit(t *testing.T) {
	for _, explicitError := range []bool{false, true} {
		name := "stop_acknowledged_but_alive"
		if explicitError {
			name = "stop_failed"
		}
		t.Run(name, func(t *testing.T) {
			worktree := t.TempDir()
			handle := &acknowledgedStopHandle{}
			if explicitError {
				handle.stopErr = errors.New("resource refuses stop")
			}
			var replacements atomic.Int32
			p := project.Define(func(_ context.Context, b *project.Builder) error {
				b.Name("restart-ownership")
				svc := b.Service("server").Run(func(_ context.Context, rt *project.Runtime) error {
					replacements.Add(1)
					rt.RegisterServiceHandle(newGenericServiceHandle())
					return nil
				})
				b.Target("dev", svc)
				return nil
			})
			eng, err := New(p, worktree)
			if err != nil {
				t.Fatal(err)
			}
			req := Request{Target: "dev", Worktree: worktree, Mode: api.ModeDev}
			_, state, rt, err := eng.prepareExecution(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			state.registerService("server", handle)
			state.setNodeState("server", api.StateRunning, "", "", 0)
			before, _ := state.serviceSnapshot("server")
			result, err := eng.applyServiceLifecycleCommand(context.Background(), req, state, rt, serviceLifecycleCommand{task: "server", action: "restart"})
			if err == nil || replacements.Load() != 0 || result.Ready || result.Stopped {
				t.Errorf("restart replaced an unconfirmed resource: replacements=%d result=%+v err=%v", replacements.Load(), result, err)
			}
			after, ok := state.serviceSnapshot("server")
			if !ok || after.handle != before.handle || after.generation != before.generation {
				t.Error("failed stop lost the original resource generation")
			}
			if state.statusSnapshot()["server"].State != api.StateDegraded {
				t.Error("failed restart did not retain degraded resource status")
			}
			// Clean any replacement admitted by a regressed implementation.
			if ok && after.handle != handle {
				_ = after.handle.Stop()
			}
		})
	}
}

type ownershipFingerprintProject struct {
	configured  *atomic.Int32
	fingerprint project.FingerprintFunc
}

func (p ownershipFingerprintProject) Name() string { return "fingerprint-ownership" }
func (p ownershipFingerprintProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	p.configured.Add(1)
	return project.InstanceConfig{Label: "fingerprint-ownership"}, nil
}
func (p ownershipFingerprintProject) Tasks() []project.Task {
	return []project.Task{{Name: "build", Kind: project.KindOnce, Cache: true,
		Inputs:  project.Inputs{Custom: []project.FingerprintFunc{p.fingerprint}},
		Outputs: project.Outputs{Files: []string{"artifact.txt"}},
		Run:     func(context.Context, *project.Runtime) error { return nil },
	}}
}
func (p ownershipFingerprintProject) Targets() []project.Target {
	return []project.Target{{Name: "verify", RootTasks: []string{"build"}}}
}

func TestCacheKeyOwnershipRejectsBeforeConfigurationAndFingerprintCallbacks(t *testing.T) {
	worktree := t.TempDir()
	lease, err := execution.Acquire(worktree, execution.Owner{Target: "dev", Mode: "watch"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	var configured, fingerprinted atomic.Int32
	eng, err := New(ownershipFingerprintProject{configured: &configured, fingerprint: func(context.Context, *project.Runtime) (string, error) {
		fingerprinted.Add(1)
		return "fingerprint", nil
	}}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = eng.CacheKeyWithManifest(context.Background(), Request{Target: "verify", Worktree: worktree, Mode: api.ModeCI})
	var conflict *execution.ConflictError
	if !errors.As(err, &conflict) {
		t.Errorf("cache planning bypassed execution ownership: %v", err)
	}
	if configured.Load() != 0 || fingerprinted.Load() != 0 {
		t.Errorf("rejected cache planning invoked callbacks: configured=%d fingerprinted=%d", configured.Load(), fingerprinted.Load())
	}
}

func TestCacheKeyOwnershipRetainsResourceRegisteredByFailedFingerprint(t *testing.T) {
	worktree := t.TempDir()
	var configured atomic.Int32
	handle := &acknowledgedStopHandle{}
	failure := errors.New("fingerprint failed after registering resource")
	eng, err := New(ownershipFingerprintProject{configured: &configured, fingerprint: func(_ context.Context, rt *project.Runtime) (string, error) {
		rt.RegisterServiceHandle(handle)
		return "", failure
	}}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = eng.CacheKeyWithManifest(context.Background(), Request{Target: "verify", Worktree: worktree, Mode: api.ModeCI})
	if !errors.Is(err, failure) {
		t.Fatalf("lost fingerprint failure: %v", err)
	}
	if handle.calls.Load() == 0 {
		t.Error("cache planning never attempted registered resource cleanup")
	}
	contender, err := execution.Acquire(worktree, execution.Owner{Target: "replacement"})
	if contender != nil {
		_ = contender.Release()
		t.Error("failed fingerprint left a live resource but permitted replacement")
	}
	var conflict *execution.ConflictError
	if !errors.As(err, &conflict) || !conflict.RecoveryRequired {
		t.Errorf("failed fingerprint did not retain recovery evidence: %v", err)
	}
}
