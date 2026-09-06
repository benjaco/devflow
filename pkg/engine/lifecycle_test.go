package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func TestRunFailureStopsRegisteredServices(t *testing.T) {
	for _, failure := range []string{"dependent task", "service startup"} {
		t.Run(failure, func(t *testing.T) {
			isolateEngineUserCache(t)
			worktree := t.TempDir()
			handle := newGenericServiceHandle()
			t.Cleanup(func() { _ = handle.Stop() })
			failureErr := errors.New("startup fixture failed")
			p := project.Define(func(_ context.Context, b *project.Builder) error {
				b.Name("service-failure-cleanup")
				svc := b.Service("managed").Run(func(_ context.Context, rt *project.Runtime) error {
					rt.RegisterServiceHandle(handle)
					if failure == "service startup" {
						return failureErr
					}
					return nil
				})
				check := b.Task("check").DependsOn(svc).Run(func(context.Context, *project.Runtime) error {
					return failureErr
				})
				b.Target("check", check)
				return nil
			})
			eng, err := New(p, worktree)
			if err != nil {
				t.Fatal(err)
			}
			out, err := eng.Run(context.Background(), Request{Target: "check", Worktree: worktree, Mode: api.ModeCI})
			if !errors.Is(err, failureErr) {
				t.Fatalf("run error = %v, want original failure", err)
			}
			if handle.Alive() {
				t.Fatal("failed run leaked its registered service")
			}
			state, err := instance.LoadStatus(worktree, out.Result.InstanceID)
			if err != nil {
				t.Fatal(err)
			}
			wantState := api.StateStopped
			if failure == "service startup" {
				wantState = api.StateFailed
			}
			if got := state.Nodes["managed"].State; got != wantState {
				t.Fatalf("managed service state = %s, want %s", got, wantState)
			}
			if out.Result.FailedNode == "" || out.Result.Success {
				t.Fatalf("cleanup lost the actionable failure: %+v", out.Result)
			}
		})
	}
}

// A service can become dead before its Wait call finishes draining diagnostics.
// Hold Wait to make the successful-readiness/failed-service race deterministic.
type drainingServiceHandle struct {
	*genericServiceHandle
	drained <-chan struct{}
}

func (h drainingServiceHandle) Wait() error {
	<-h.drained
	return h.genericServiceHandle.Wait()
}

func TestServiceReadinessDoesNotCommitAfterServiceExit(t *testing.T) {
	drained := make(chan struct{})
	defer close(drained)
	handle := drainingServiceHandle{genericServiceHandle: newGenericServiceHandle(), drained: drained}
	defer handle.Stop()
	var committed bool
	task := project.Task{
		Ready: func(context.Context, *project.Runtime) error { return handle.Stop() },
		AfterReady: func(context.Context, *project.Runtime) error {
			committed = true
			return nil
		},
	}
	err := (&Engine{}).awaitServiceReady(context.Background(), &project.Runtime{}, task, handle)
	var earlyExit *serviceEarlyExitError
	if !errors.As(err, &earlyExit) || committed {
		t.Fatalf("dead service passed readiness: error=%v afterReady=%t", err, committed)
	}
}

func TestFlushReadinessStopsWaitingWhenServiceExits(t *testing.T) {
	handle := newGenericServiceHandle()
	defer handle.Stop()
	started := make(chan struct{})
	canceled := make(chan struct{})
	task := project.Task{
		Name:         "managed",
		Kind:         project.KindService,
		ReadyTimeout: time.Minute,
		Ready: func(ctx context.Context, _ *project.Runtime) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	worktree := t.TempDir()
	state := &runState{
		inst:     &api.Instance{ID: "test"},
		services: map[string]project.ServiceHandle{"managed": handle},
	}
	result := make(chan api.FlushService, 1)
	go func() {
		result <- (&Engine{}).evaluateFlushService(ctx, Request{Worktree: worktree}, &project.Runtime{}, state, task, api.NodeStatus{State: api.StateRunning})
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("flush readiness did not start")
	}
	_ = handle.Stop()
	select {
	case service := <-result:
		if service.Ready || service.Alive || !strings.Contains(service.Error, "not alive") {
			t.Fatalf("flush did not report the dead service: %+v", service)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("flush kept waiting on a dead service's readiness callback")
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("flush left its readiness callback running")
	}
}

func TestFlushReadinessEnforcesTimeout(t *testing.T) {
	handle := newGenericServiceHandle()
	defer handle.Stop()
	finish := make(chan struct{})
	defer close(finish)
	task := project.Task{
		Name:         "managed",
		Kind:         project.KindService,
		ReadyTimeout: 25 * time.Millisecond,
		Ready: func(context.Context, *project.Runtime) error {
			<-finish
			return nil
		},
	}
	state := &runState{
		inst:     &api.Instance{ID: "test"},
		services: map[string]project.ServiceHandle{"managed": handle},
	}
	worktree := t.TempDir()
	result := make(chan api.FlushService, 1)
	go func() {
		result <- (&Engine{}).evaluateFlushService(context.Background(), Request{Worktree: worktree}, &project.Runtime{}, state, task, api.NodeStatus{State: api.StateRunning})
	}()
	select {
	case service := <-result:
		if service.Ready || !service.Alive || !strings.Contains(service.Error, "deadline exceeded") {
			t.Fatalf("flush did not enforce readiness timeout: %+v", service)
		}
	case <-time.After(time.Second):
		t.Fatal("readiness callback blocked flush past its deadline")
	}
}

func TestLifecycleRestartEarlyExitCancelsReadinessAndPreservesIndependentService(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	frontend := newGenericServiceHandle()
	defer frontend.Stop()
	var starts atomic.Int32
	readyCanceled := make(chan struct{})
	p := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("restart-early-exit")
		backend := b.Service("backend").Run(func(_ context.Context, rt *project.Runtime) error {
			if starts.Add(1) == 1 {
				rt.RegisterServiceHandle(newGenericServiceHandle())
			} else {
				rt.EmitLogLine("stderr", "backend bind failed during restart")
				rt.RegisterServiceHandle(earlyExitServiceHandle{err: errors.New("exit status 17")})
			}
			return nil
		}).Ready(func(ctx context.Context, _ *project.Runtime) error {
			if starts.Load() == 1 {
				return nil
			}
			<-ctx.Done()
			close(readyCanceled)
			return ctx.Err()
		}).ReadyTimeout(time.Minute)
		other := b.Service("frontend").Run(func(_ context.Context, rt *project.Runtime) error {
			rt.RegisterServiceHandle(frontend)
			return nil
		})
		b.Target("dev", backend, other)
		return nil
	})
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	controller := NewLifecycleController()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := eng.Run(ctx, Request{Target: "dev", Worktree: worktree, Mode: api.ModeDev, LifecycleController: controller})
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("engine did not stop after restart test")
		}
	})
	restartCtx, restartCancel := context.WithTimeout(ctx, 2*time.Second)
	defer restartCancel()
	result, err := controller.Restart(restartCtx, "backend")
	if err == nil || !strings.Contains(err.Error(), "exit status 17") || result.Ready {
		t.Fatalf("restart failed to report early service exit: result=%+v error=%v", result, err)
	}
	select {
	case <-readyCanceled:
	case <-restartCtx.Done():
		t.Fatal("restart did not cancel its readiness callback")
	}
	if !frontend.Alive() {
		t.Fatal("failed restart stopped the independent frontend")
	}
	id, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	status, err := instance.LoadStatus(worktree, id)
	if err != nil {
		t.Fatal(err)
	}
	if backend := status.Nodes["backend"]; backend.State != api.StateFailed || len(backend.FailureExcerpts) == 0 {
		t.Fatalf("failed restart omitted terminal diagnostics: %+v", backend)
	}
}
