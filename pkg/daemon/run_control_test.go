package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

func TestRunRequestDeadlineCancelsExecution(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(map[bool]string{false: "attached", true: "detached"}[detached], func(t *testing.T) {
			started := make(chan struct{})
			canceled := make(chan struct{})
			s := runControlServer(t, "daemon-request-deadline-"+t.Name(), func(ctx context.Context, _ *project.Runtime) error {
				close(started)
				<-ctx.Done()
				close(canceled)
				return ctx.Err()
			})
			done := make(chan Response, 1)
			go func() {
				done <- s.handleRequest(context.Background(), Request{Action: ActionRun, Target: "check", Mode: api.ModeCI, Detach: detached, TimeoutMs: 500})
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("task did not start")
			}
			select {
			case <-canceled:
			case <-time.After(2 * time.Second):
				t.Error("operation deadline did not reach the task context")
				s.stopActive(3 * time.Second)
			}
			select {
			case response := <-done:
				if !detached && (response.Error == nil || response.Error.Code != "deadline_exceeded") {
					t.Errorf("deadline response = %+v", response)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("operation did not finish after cancellation")
			}
		})
	}
}

func TestInvalidRunControlsLeaveActiveRunUntouched(t *testing.T) {
	for _, request := range []Request{
		{Action: ActionWatch, Target: "check", Headless: "guess"},
		{Action: ActionRun, Target: "check", TimeoutMs: -1},
		{Action: ActionRunAction, ActionID: "check", TimeoutMs: 1<<63 - 1},
	} {
		t.Run(string(request.Action), func(t *testing.T) {
			s := runControlServer(t, "invalid-daemon-controls-"+t.Name(), func(context.Context, *project.Runtime) error {
				t.Error("invalid request executed")
				return nil
			})
			var stops atomic.Int32
			done := make(chan struct{})
			s.active = &activeRun{done: done, cancel: sync.OnceFunc(func() { stops.Add(1); close(done) })}
			response := s.handleRequest(context.Background(), request)
			if response.OK || response.Error == nil || response.Error.Code != "invalid_arguments" {
				t.Fatalf("invalid controls response: %+v", response)
			}
			if stops.Load() != 0 {
				t.Fatal("invalid controls stopped the current owner")
			}
		})
	}
}

func TestQueuedRunCancellationPreservesCurrentWatcher(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	s := runControlServer(t, "daemon-queued-run-cancellation", func(ctx context.Context, _ *project.Runtime) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	})
	watch := s.handleRequest(context.Background(), Request{Action: ActionWatch, Target: "check"})
	if !watch.OK || watch.Started == nil {
		t.Fatalf("start watcher: %+v", watch)
	}
	<-started
	s.transitionMu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			s.transitionMu.Unlock()
		}
	}()
	finished := make(chan Response, 1)
	go func() {
		finished <- s.handleRequest(context.Background(), Request{Action: ActionRun, Target: "check", Mode: api.ModeCI})
	}()
	var queued api.RunRecord
	deadline := time.Now().Add(3 * time.Second)
	for queued.RunID == "" && time.Now().Before(deadline) {
		records, err := instance.ListRuns(s.worktree, s.instanceID)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			if record.Mode == api.ModeCI && record.State == api.RunQueued {
				queued = record
			}
		}
		if queued.RunID == "" {
			time.Sleep(time.Millisecond)
		}
	}
	if queued.RunID == "" {
		t.Fatal("queued operation was not inspectable before admission")
	}
	if err := instance.RequestRunCancellation(context.Background(), s.worktree, s.instanceID, queued.RunID); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-finished:
		if response.OK || response.Error == nil || response.Error.Code != "operation_cancelled" {
			t.Fatalf("queued cancellation response = %+v", response)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued cancellation waited for an unrelated transition to finish")
	}
	s.transitionMu.Unlock()
	unlocked = true
	select {
	case <-canceled:
		t.Fatal("canceling the queued run stopped the development watcher")
	default:
	}
	record, err := instance.LoadRun(s.worktree, s.instanceID, queued.RunID)
	if err != nil || record.State != api.RunCanceled {
		t.Fatalf("queued cancellation evidence: %+v err=%v", record, err)
	}
	if err := instance.RequestRunCancellation(context.Background(), s.worktree, s.instanceID, queued.RunID); err == nil {
		t.Fatal("completed run accepted a fresh cancellation")
	}
	if !s.stopActive(3 * time.Second) {
		t.Fatal("watcher did not stop during cleanup")
	}
}

func TestQueuedDeadlineReturnsBeforeMutationAdmission(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(map[bool]string{false: "attached", true: "detached"}[detached], func(t *testing.T) {
			s := runControlServer(t, "daemon-queued-deadline-"+t.Name(), func(context.Context, *project.Runtime) error {
				t.Error("expired queued operation executed")
				return nil
			})
			s.transitionMu.Lock()
			defer s.transitionMu.Unlock()
			finished := make(chan Response, 1)
			go func() {
				finished <- s.handleRequest(context.Background(), Request{Action: ActionRun, Target: "check", Mode: api.ModeCI, Detach: detached, TimeoutMs: 50})
			}()
			select {
			case response := <-finished:
				if response.OK || response.Error == nil || response.Error.Code != "deadline_exceeded" {
					t.Fatalf("queued deadline response = %+v", response)
				}
			case <-time.After(time.Second):
				t.Fatal("queued deadline waited for mutation admission")
			}
		})
	}
}

func TestActionAndExecutionShareRunIdentityAndCancellation(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	name := "daemon-action-run-control"
	project.Register(project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name(name)
		task := b.Task("action").NoCache().Run(func(ctx context.Context, _ *project.Runtime) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
		b.Target("up", task)
		b.Action("wait").Task(task)
		return nil
	}))
	s := &Server{worktree: worktree, instanceID: inst.ID, projectName: name, subscribers: map[chan api.Event]bool{}}
	t.Cleanup(func() { s.stopActive(3 * time.Second) })
	finished := make(chan Response, 1)
	go func() {
		finished <- s.handleRequest(context.Background(), Request{Action: ActionRunAction, ActionID: "wait"})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("action did not start")
	}
	records, err := instance.ListRuns(worktree, inst.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("action run records: %+v err=%v", records, err)
	}
	if err := instance.RequestRunCancellation(context.Background(), worktree, inst.ID, records[0].RunID); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-finished:
		if response.OK || response.ActionResult == nil || response.Run == nil {
			t.Fatalf("action cancellation = %+v", response)
		}
		if response.ActionResult.RunID != response.Run.RunID || response.Run.RunID != records[0].RunID {
			t.Fatalf("action created competing identities: action=%s task=%s record=%s", response.ActionResult.RunID, response.Run.RunID, records[0].RunID)
		}
		if !errors.Is(response.Error, context.Canceled) && response.Error.Code != "operation_cancelled" {
			t.Fatalf("action cancellation error: %+v", response.Error)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("action did not finish after its run was canceled")
	}
}

func TestWaitingActionPromptRemainsInspectableAndAnswerable(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	name := "daemon-action-prompt-control"
	project.Register(project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name(name)
		task := b.Task("action").NoCache().Run(func(_ context.Context, rt *project.Runtime) error {
			answer, err := rt.OnPrompt(rt.TaskName, process.PromptRequest{Kind: process.PromptConfirm, Prompt: "Continue?"})
			if err == nil && answer.Value != "y" {
				return errors.New("expected confirmation")
			}
			return err
		})
		b.Target("up", task)
		b.Action("confirm").Task(task)
		return nil
	}))
	s := &Server{worktree: worktree, instanceID: inst.ID, projectName: name, subscribers: map[chan api.Event]bool{}}
	t.Cleanup(func() { s.stopActive(3 * time.Second) })
	finished := make(chan Response, 1)
	go func() {
		finished <- s.handleRequest(context.Background(), Request{Action: ActionRunAction, ActionID: "confirm", Headless: api.HeadlessWait, TimeoutMs: 5000})
	}()
	var prompt api.Prompt
	deadline := time.Now().Add(3 * time.Second)
	for prompt.ID == "" && time.Now().Before(deadline) {
		status := s.handleRequest(context.Background(), Request{Action: ActionStatus})
		if !status.OK {
			t.Fatal(status.Error)
		}
		if len(status.Status.PendingPrompts) > 0 {
			prompt = status.Status.PendingPrompts[0]
			if prompt.RunID != status.Status.RunID {
				t.Fatalf("prompt belongs to another displayed run: %+v", status.Status)
			}
		}
		if prompt.ID == "" {
			time.Sleep(time.Millisecond)
		}
	}
	if prompt.ID == "" {
		t.Fatal("foreground action's pending prompt was not exposed in status")
	}
	confirmed := true
	if err := instance.RespondPrompt(context.Background(), worktree, inst.ID, api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Confirm: &confirmed}); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-finished:
		if !response.OK || response.Run == nil || !response.Run.Success {
			t.Fatalf("answer did not resume the waiting action: %+v", response)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("answer blocked behind the foreground action")
	}
}

func TestDetachedObserverDisconnectKeepsOwnedRun(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	s := runControlServer(t, "daemon-detached-observer-disconnect", func(ctx context.Context, _ *project.Runtime) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	})
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	handlerDone := make(chan struct{})
	go func() {
		s.handleConn(context.Background(), serverConn)
		close(handlerDone)
	}()
	if err := json.NewEncoder(clientConn).Encode(Request{Action: ActionWatch, Target: "check"}); err != nil {
		t.Fatal(err)
	}
	var response frame
	if err := json.NewDecoder(clientConn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Response == nil || !response.Response.OK {
		t.Fatalf("watch acceptance = %+v", response)
	}
	_ = clientConn.Close()
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("observer connection did not finish")
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("detached execution did not start")
	}
	select {
	case <-canceled:
		t.Fatal("observer disconnect canceled detached execution")
	default:
	}
}

func TestDetachedRunReturnsStableExecutionIdentity(t *testing.T) {
	started := make(chan struct{})
	s := runControlServer(t, "daemon-detached-run-identity", func(ctx context.Context, _ *project.Runtime) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	first := s.handleRequest(context.Background(), Request{Action: ActionWatch, Target: "check"})
	if !first.OK || first.Started == nil {
		t.Fatalf("start: %+v", first)
	}
	<-started
	second := s.handleRequest(context.Background(), Request{Action: ActionWatch, Target: "check"})
	if !second.OK || second.Started == nil {
		t.Fatalf("ensure: %+v", second)
	}
	var firstFields, secondFields map[string]any
	firstJSON, _ := json.Marshal(first.Started)
	secondJSON, _ := json.Marshal(second.Started)
	_ = json.Unmarshal(firstJSON, &firstFields)
	_ = json.Unmarshal(secondJSON, &secondFields)
	if firstFields["runId"] == nil || firstFields["runId"] == "" {
		t.Fatal("detached acceptance omitted the execution run ID")
	}
	if secondFields["runId"] != firstFields["runId"] {
		t.Fatalf("idempotent start changed run identity: %v -> %v", firstFields["runId"], secondFields["runId"])
	}
}

func TestDetachedReadinessCannotReusePreviousRun(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.SaveStatus(worktree, inst.ID, "dev", api.ModeWatch, map[string]api.NodeStatus{
		"build": {Name: "build", RunID: "previous", State: api.StateDone},
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{worktree: worktree, instanceID: inst.ID, active: &activeRun{runID: "current"}}
	if ready, state := s.detachedTargetState("dev", "current"); ready || state != "starting" {
		t.Fatalf("previous run's readiness reused: ready=%v state=%s", ready, state)
	}
}

func runControlServer(t *testing.T, name string, run project.RunFunc) *Server {
	t.Helper()
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	project.Register(daemonTestProject{name: name,
		tasks:   []project.Task{{Name: "check", Kind: project.KindOnce, Run: run}},
		targets: []project.Target{{Name: "check", RootTasks: []string{"check"}}},
	})
	s := &Server{worktree: worktree, instanceID: inst.ID, projectName: name, subscribers: map[chan api.Event]bool{}}
	t.Cleanup(func() { s.stopActive(3 * time.Second) })
	return s
}
