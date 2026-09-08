package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func attemptCompletions(events []api.Event) []api.Event {
	var completed []api.Event
	for _, event := range events {
		if event.Type == api.EventTaskAttemptFinished {
			completed = append(completed, event)
		}
	}
	return completed
}

func TestAttemptCompletionRetainsFinalCallbackOutputAndCacheEvidence(t *testing.T) {
	isolateEngineUserCache(t)
	root := t.TempDir()
	var calls atomic.Int32
	p := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("attempt-output")
		shared := b.Task("shared").OutputFiles("artifact.txt").BeforeRun(func(_ context.Context, rt *project.Runtime) error {
			rt.EmitLogLine("stdout", "before hook")
			return nil
		}).Run(func(_ context.Context, rt *project.Runtime) error {
			calls.Add(1)
			defer rt.EmitLogLine("stdout", "final callback output")
			return os.WriteFile(rt.Abs("artifact.txt"), []byte("artifact"), 0o600)
		})
		left := b.Task("left").NoCache().DependsOn(shared)
		right := b.Task("right").NoCache().DependsOn(shared)
		b.Target("verify", b.Group("all", left, right))
		return nil
	})
	eng, err := New(p, root)
	if err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEvents()
	for run := range 2 {
		outcome, err := eng.Run(context.Background(), Request{Target: "verify", Worktree: root, Mode: api.ModeCI, MaxParallel: 2})
		if err != nil {
			t.Fatal(err)
		}
		completed := attemptCompletions(drainEvents(events))
		if len(completed) != 3 {
			t.Fatalf("run %d completed %d attempts, want shared/left/right and no group attempt: %+v", run, len(completed), completed)
		}
		record, err := instance.LoadRun(root, outcome.Instance.ID, outcome.Result.RunID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range completed {
			attempt := event.Attempt
			if attempt == nil || !attempt.LogsComplete || attempt.FinishedAt.IsZero() || attempt.FinishedAt.Before(attempt.StartedAt) {
				t.Fatalf("completion lacks closed log and execution timing evidence: %+v", event)
			}
			if event.RunID != record.RunID || event.AttemptID != attempt.AttemptID || event.Task != attempt.Task || event.State != attempt.State {
				t.Fatalf("completion identity/state mismatch: %+v", event)
			}
			var retained *api.TaskAttempt
			for i := range record.Attempts {
				if record.Attempts[i].AttemptID == attempt.AttemptID {
					retained = &record.Attempts[i]
				}
			}
			if retained == nil || !retained.LogsComplete || !retained.FinishedAt.Equal(attempt.FinishedAt) || retained.State != attempt.State || retained.CacheOutcome != attempt.CacheOutcome {
				t.Fatalf("completion differs from final retained evidence: event=%+v retained=%+v", attempt, retained)
			}
			if attempt.Task != "shared" {
				if attempt.CacheOutcome != "" {
					t.Fatalf("uncached execution reported a cache outcome: %+v", attempt)
				}
				continue
			}
			if want := []string{"miss", "hit"}[run]; attempt.CacheOutcome != want {
				t.Fatalf("cache outcome=%q want %q in completed attempt", attempt.CacheOutcome, want)
			}
			data, err := os.ReadFile(attempt.LogPath)
			if err != nil {
				t.Fatal(err)
			}
			if run == 0 && (!attempt.Executed || !strings.Contains(string(data), "before hook\nstdout: final callback output\n")) {
				t.Fatalf("completion lost last callback bytes: attempt=%+v log=%q", attempt, data)
			}
			if run == 1 && (attempt.Executed || attempt.State != api.StateCached || strings.Contains(string(data), "callback output") || strings.Contains(string(data), "before hook")) {
				t.Fatalf("cache hit was reported as execution: attempt=%+v log=%q", attempt, data)
			}
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("shared task executed %d times, want once plus cache reuse", calls.Load())
	}
}

// Stop can confirm a dead resource while Wait is still draining its last log.
type attemptDrainHandle struct {
	stopped chan struct{}
	release chan struct{}
	done    chan struct{}
	stop    sync.Once
	drain   sync.Once
	rt      *project.Runtime
}

func (h *attemptDrainHandle) PID() int { return 0 }
func (h *attemptDrainHandle) Alive() bool {
	select {
	case <-h.stopped:
		return false
	default:
		return true
	}
}
func (h *attemptDrainHandle) Stop() error {
	h.stop.Do(func() { close(h.stopped) })
	return nil
}
func (h *attemptDrainHandle) Wait() error {
	<-h.stopped
	<-h.release
	h.drain.Do(func() {
		h.rt.EmitLogLine("stdout", "final drained service output")
		close(h.done)
	})
	return nil
}

func TestAttemptCompletionWaitsForServiceDrainAndFinalStartupState(t *testing.T) {
	for _, startupFailure := range []bool{false, true} {
		name := "ready_then_stop"
		if startupFailure {
			name = "failed_startup"
		}
		t.Run(name, func(t *testing.T) {
			isolateEngineUserCache(t)
			root := t.TempDir()
			handle := &attemptDrainHandle{stopped: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
			ready := make(chan struct{})
			continueTask := make(chan struct{})
			var releaseOnce, continueOnce sync.Once
			defer releaseOnce.Do(func() { close(handle.release) })
			defer continueOnce.Do(func() { close(continueTask) })
			failure := errors.New("service startup failed")
			p := project.Define(func(_ context.Context, b *project.Builder) error {
				b.Name("attempt-service-output")
				svc := b.Service("service").Run(func(_ context.Context, rt *project.Runtime) error {
					handle.rt = rt
					rt.EmitLogLine("stdout", "service startup output")
					rt.RegisterServiceHandle(handle)
					if startupFailure {
						return failure
					}
					return nil
				})
				check := b.Task("check").NoCache().DependsOn(svc).Run(func(ctx context.Context, _ *project.Runtime) error {
					close(ready)
					select {
					case <-continueTask:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
				b.Target("verify", check)
				return nil
			})
			eng, err := New(p, root)
			if err != nil {
				t.Fatal(err)
			}
			events := eng.SubscribeEvents()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			type result struct {
				outcome *Outcome
				err     error
			}
			done := make(chan result, 1)
			go func() {
				outcome, err := eng.Run(ctx, Request{Target: "verify", Worktree: root, Mode: api.ModeCI})
				done <- result{outcome, err}
			}()
			if !startupFailure {
				select {
				case <-ready:
				case <-ctx.Done():
					t.Fatal("service never became ready")
				}
				if completed := attemptCompletions(drainEvents(events)); len(completed) != 0 {
					t.Fatalf("service readiness closed its log: %+v", completed)
				}
				continueOnce.Do(func() { close(continueTask) })
			}
			select {
			case <-handle.stopped:
			case <-ctx.Done():
				t.Fatal("service cleanup did not begin")
			}
			before := drainEvents(events)
			for _, event := range attemptCompletions(before) {
				if event.Task == "service" {
					t.Fatalf("dead resource completed before Wait drained: %+v", event)
				}
			}
			releaseOnce.Do(func() { close(handle.release) })
			var got result
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("service run did not complete after drain")
			}
			if startupFailure != errors.Is(got.err, failure) || got.outcome == nil || got.outcome.Result.Success == startupFailure {
				t.Fatalf("presentation evidence changed execution outcome: %+v err=%v", got.outcome, got.err)
			}
			completed := attemptCompletions(append(before, drainEvents(events)...))
			var svc []api.Event
			for _, event := range completed {
				if event.Task == "service" {
					svc = append(svc, event)
				}
			}
			if len(svc) != 1 || svc[0].Attempt == nil || !svc[0].Attempt.LogsComplete {
				t.Fatalf("service did not emit exactly one final completion: %+v", svc)
			}
			want := api.StateStopped
			if startupFailure {
				want = api.StateFailed
			}
			if svc[0].State != want || svc[0].Attempt.State != want {
				t.Fatalf("service emitted intermediate cleanup state, want %s: %+v", want, svc[0])
			}
			data, err := os.ReadFile(svc[0].Attempt.LogPath)
			if err != nil || !strings.Contains(string(data), "final drained service output") {
				t.Fatalf("completion omitted drained output: log=%q err=%v", data, err)
			}
		})
	}
}

func TestAttemptCompletionRetainsCanceledOutputAndRejectsLiveCleanup(t *testing.T) {
	for _, liveCleanup := range []bool{false, true} {
		name := "canceled"
		if liveCleanup {
			name = "unconfirmed_cleanup"
		}
		t.Run(name, func(t *testing.T) {
			isolateEngineUserCache(t)
			root := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			eng, err := New(ownershipProject{run: func(_ context.Context, rt *project.Runtime) error {
				defer rt.EmitLogLine("stdout", "available final output")
				if liveCleanup {
					rt.RegisterServiceHandle(&acknowledgedStopHandle{})
					return nil
				}
				cancel()
				return ctx.Err()
			}}, root)
			if err != nil {
				t.Fatal(err)
			}
			events := eng.SubscribeEvents()
			outcome, err := eng.Run(ctx, Request{Target: "verify", Worktree: root, Mode: api.ModeCI})
			if err == nil || outcome == nil || outcome.Result.Success {
				t.Fatalf("lost failed outcome: %+v err=%v", outcome, err)
			}
			record, err := instance.LoadRun(root, outcome.Result.InstanceID, outcome.Result.RunID)
			if err != nil {
				t.Fatal(err)
			}
			completed := attemptCompletions(drainEvents(events))
			if liveCleanup {
				if len(completed) != 0 || len(record.Attempts) != 1 || record.Attempts[0].LogsComplete {
					t.Fatalf("live cleanup falsely closed output: events=%+v attempts=%+v", completed, record.Attempts)
				}
				return
			}
			if len(completed) != 1 || completed[0].Attempt == nil || completed[0].State != api.StateCanceled || !completed[0].Attempt.LogsComplete {
				t.Fatalf("canceled callback lost completed output: %+v", completed)
			}
			data, err := os.ReadFile(completed[0].Attempt.LogPath)
			if err != nil || !strings.Contains(string(data), "available final output") {
				t.Fatalf("canceled log=%q err=%v", data, err)
			}
		})
	}
}

func TestAttemptOutputDrainHonorsCancellationWithoutClosingEvidence(t *testing.T) {
	stopped := make(chan struct{})
	close(stopped)
	handle := &attemptDrainHandle{stopped: stopped}
	unfinished := make(chan struct{})
	state := &runState{attemptOutputs: map[string]*attemptOutput{
		"attempt": {callbackReturned: true, resources: []attemptResource{{handle: handle, done: unfinished}}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	returned := make(chan struct{})
	go func() {
		state.drainAttemptOutputContext(ctx, "attempt")
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		close(unfinished)
		<-returned
		t.Fatal("log drain ignored its deadline and kept cleanup waiting")
	}
	select {
	case <-unfinished:
		t.Fatal("cancellation fabricated writer completion")
	default:
	}
	if state.attemptOutputs["attempt"] == nil {
		t.Fatal("timed-out output lost its incomplete attempt identity")
	}
}

func TestAttemptCompletionDoesNotCloseLogBeforeTimedOutReadinessReturns(t *testing.T) {
	isolateEngineUserCache(t)
	root := t.TempDir()
	readyStarted := make(chan struct{})
	releaseReady := make(chan struct{})
	readyReturned := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseReady) })
		select {
		case <-readyReturned:
		case <-time.After(time.Second):
			t.Error("late readiness callback did not exit")
		}
	})
	handle := newGenericServiceHandle()
	p := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("late-readiness-output")
		svc := b.Service("service").Run(func(_ context.Context, rt *project.Runtime) error {
			rt.RegisterServiceHandle(handle)
			return nil
		}).Ready(func(_ context.Context, rt *project.Runtime) error {
			defer close(readyReturned)
			close(readyStarted)
			<-releaseReady
			rt.EmitLogLine("stdout", "late readiness output")
			return nil
		}).ReadyTimeout(time.Millisecond)
		b.Target("verify", svc)
		return nil
	})
	eng, err := New(p, root)
	if err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEvents()
	type result struct {
		outcome *Outcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := eng.Run(context.Background(), Request{Target: "verify", Worktree: root, Mode: api.ModeCI})
		done <- result{outcome, err}
	}()
	select {
	case <-readyStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("readiness callback did not start")
	}
	var got result
	select {
	case got = <-done:
	case <-time.After(time.Second):
		t.Fatal("timed-out readiness kept the operation waiting for its callback")
	}
	if got.err == nil || got.outcome == nil || got.outcome.Result.Success || handle.Alive() {
		t.Fatalf("readiness timeout lost execution failure/cleanup: outcome=%+v err=%v alive=%t", got.outcome, got.err, handle.Alive())
	}
	record, err := instance.LoadRun(root, got.outcome.Result.InstanceID, got.outcome.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	completed := attemptCompletions(drainEvents(events))
	if len(record.Attempts) != 1 || record.Attempts[0].LogsComplete || len(completed) != 0 {
		t.Fatalf("readiness still owns a writer after apparent task completion: attempts=%+v events=%+v", record.Attempts, completed)
	}
	releaseOnce.Do(func() { close(releaseReady) })
	<-readyReturned
	data, err := os.ReadFile(record.Attempts[0].LogPath)
	if err != nil || !strings.Contains(string(data), "late readiness output") {
		t.Fatalf("late output unavailable: log=%q err=%v", data, err)
	}
	retained, err := instance.LoadRun(root, record.InstanceID, record.RunID)
	if err != nil || retained.Attempts[0].LogsComplete || len(attemptCompletions(drainEvents(events))) != 0 {
		t.Fatalf("late callback changed final evidence: retained=%+v err=%v", retained, err)
	}
}
