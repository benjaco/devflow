package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/execution"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func TestConcurrentStartAdmitsOnlyOneReplacement(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	name := "daemon-concurrent-replacement"
	project.Register(daemonTestProject{name: name,
		tasks:   []project.Task{{Name: "check", Kind: project.KindOnce, Run: func(context.Context, *project.Runtime) error { starts.Add(1); return nil }}},
		targets: []project.Target{{Name: "new", RootTasks: []string{"check"}}},
	})
	var stops atomic.Int32
	stopEntered := make(chan struct{}, 2)
	oldDone := make(chan struct{})
	old := &activeRun{projectName: name, target: "old", mode: api.ModeWatch, done: oldDone, cancel: func() { stops.Add(1); stopEntered <- struct{}{} }}
	s := &Server{worktree: worktree, instanceID: inst.ID, projectName: name, active: old, subscribers: map[chan api.Event]bool{}}
	results := make(chan error, 2)
	go func() { _, err := s.startActive(context.Background(), name, "new", api.ModeWatch, 1); results <- err }()
	<-stopEntered
	go func() { _, err := s.startActive(context.Background(), name, "new", api.ModeWatch, 1); results <- err }()
	select {
	case <-stopEntered:
		t.Error("concurrent start entered the same stop/replace transition twice")
	case <-time.After(100 * time.Millisecond):
	}
	close(oldDone)
	defer s.stopActive(3 * time.Second)
	for range 2 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for starts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stops.Load() != 1 || starts.Load() != 1 {
		t.Fatalf("replacement must run once: stops=%d starts=%d", stops.Load(), starts.Load())
	}
}

func TestReplacementTimeoutPreservesActiveOwnerAndMetadata(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	name := "daemon-replacement-timeout"
	project.Register(daemonTestProject{name: name,
		tasks:   []project.Task{{Name: "check", Kind: project.KindOnce, Run: func(context.Context, *project.Runtime) error { return nil }}},
		targets: []project.Target{{Name: "new", RootTasks: []string{"check"}}},
	})
	inst.LastRun = api.RunConfig{Project: name, Target: "old", Mode: api.ModeWatch, Detached: true}
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(worktree, ".devflow", "state", "instances", inst.ID, "instance.json"))
	if err != nil {
		t.Fatal(err)
	}
	oldDone := make(chan struct{})
	old := &activeRun{projectName: name, target: "old", mode: api.ModeWatch, done: oldDone, cancel: func() {}}
	s := &Server{worktree: worktree, instanceID: inst.ID, projectName: name, active: old, subscribers: map[chan api.Event]bool{}}
	_, err = s.startActive(context.Background(), name, "new", api.ModeWatch, 1)
	if err == nil || !strings.Contains(err.Error(), "stop") {
		t.Errorf("replacement after incomplete stop must fail, got %v", err)
	}
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active != old {
		t.Error("replacement discarded ownership of the still-running engine")
	}
	after, readErr := os.ReadFile(filepath.Join(worktree, ".devflow", "state", "instances", inst.ID, "instance.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(before) != string(after) {
		t.Error("failed replacement rewrote instance metadata")
	}
	close(oldDone)
	s.stopActive(3 * time.Second)
}

func TestFailedDetachedEngineInitializationReleasesActiveSlot(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	name := "daemon-invalid-engine-cleanup"
	project.Register(daemonTestProject{name: name,
		tasks:   []project.Task{{Name: "invalid", Kind: project.KindOnce, Cache: true}},
		targets: []project.Target{{Name: "invalid", RootTasks: []string{"invalid"}}},
	})
	s := &Server{worktree: worktree, instanceID: inst.ID, projectName: name, subscribers: map[chan api.Event]bool{}, logPath: filepath.Join(worktree, "daemon.log")}
	_, err = s.startActive(context.Background(), name, "invalid", api.ModeWatch, 1)
	if err != nil {
		return
	} // synchronous validation is also correct
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if active == nil {
			return
		}
		select {
		case <-active.done:
			t.Fatal("completed failed engine still owns active slot")
		default:
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("failed engine did not release active slot")
}

func TestActionWaitAllowsStatusAndStopAndRestoresEnvironment(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	inst.Env["ACTION_VALUE"] = "original"
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	name := "daemon-action-responsive"
	project.Register(project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name(name)
		task := b.Task("action").NoCache().Run(func(ctx context.Context, rt *project.Runtime) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
		b.Target("up", task)
		b.Action("wait").Task(task).Input(project.ActionInput{Name: "value", Env: "ACTION_VALUE"})
		return nil
	}))
	s := &Server{worktree: worktree, instanceID: inst.ID, projectName: name, subscribers: map[chan api.Event]bool{}}
	finished := make(chan error, 1)
	go func() {
		_, err := s.runProjectAction(context.Background(), name, "wait", "", "", map[string]string{"value": "temporary"}, nil)
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("action did not start")
	}
	statusDone := make(chan Response, 1)
	go func() { statusDone <- s.handleRequest(context.Background(), Request{Action: ActionStatus}) }()
	select {
	case response := <-statusDone:
		if !response.OK {
			t.Fatal(response.Error)
		}
	case <-time.After(time.Second):
		t.Fatal("status blocked behind foreground action")
	}
	stopped := make(chan error, 1)
	go func() { _, err := s.stopWork(context.Background(), true, ""); stopped <- err }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop blocked behind foreground action")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("action did not finish after stop")
	}
	inst, err = instance.Load(worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := inst.Env["ACTION_VALUE"]; got != "original" {
		t.Fatalf("stop completed before action environment was restored: %q", got)
	}
}

func TestCompletedActionDoesNotRelaunchOverNewerTarget(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	name := "daemon-action-newer-selection"
	started, release := make(chan struct{}), make(chan struct{})
	project.Register(project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name(name)
		action := b.Task("action").NoCache().Run(func(context.Context, *project.Runtime) error { close(started); <-release; return nil })
		check := b.Task("check").NoCache().Run(func(context.Context, *project.Runtime) error { return nil })
		b.Target("old", check)
		b.Target("new", check)
		b.Action("change").Task(action).RelaunchPreviousTargetAfterSuccess()
		return nil
	}))
	inst.LastRun = api.RunConfig{Project: name, Target: "old", Mode: api.ModeWatch, Detached: true}
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}
	s := &Server{worktree: worktree, instanceID: inst.ID, projectName: name, subscribers: map[chan api.Event]bool{}}
	finished := make(chan *ActionRunResult, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := s.runProjectAction(context.Background(), name, "change", "", "", nil, nil)
		finished <- result
		errs <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("action did not start")
	}
	// Hold admission while the action finishes so a newer selection deterministically
	// wins before the action attempts its optional previous-target relaunch.
	s.transitionMu.Lock()
	s.mu.Lock()
	actionRun := s.active
	s.mu.Unlock()
	close(release)
	select {
	case <-actionRun.done:
	case <-time.After(3 * time.Second):
		s.transitionMu.Unlock()
		t.Fatal("action completion held admission lock")
	}
	_, err = s.startActiveLocked(context.Background(), name, "new", api.ModeWatch, 1)
	s.transitionMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer s.stopActive(3 * time.Second)
	var result *ActionRunResult
	select {
	case result = <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("action did not return")
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if result.Relaunch != nil {
		t.Fatalf("completed action relaunched stale target: %+v", result.Relaunch)
	}
	s.mu.Lock()
	current := s.active
	s.mu.Unlock()
	if current == nil || current.target != "new" {
		t.Fatalf("newer target lost ownership: %+v", current)
	}
}

func TestDaemonContentionPreservesExecutionState(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	name := "daemon-contention-state"
	project.Register(project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name(name)
		task := b.Task("check").NoCache().Run(func(context.Context, *project.Runtime) error { t.Error("rejected daemon task executed"); return nil })
		b.Target("up", task)
		b.Action("change").Task(task).Input(project.ActionInput{Name: "value", Env: "ACTION_VALUE"})
		return nil
	}))
	if err := instance.SaveStatus(worktree, inst.ID, "external", api.ModeCI, map[string]api.NodeStatus{"check": {Name: "check", State: api.StateRunning}}); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(worktree, ".devflow", "state", "instances", inst.ID, "instance.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := execution.Acquire(worktree, execution.Owner{Kind: "ci", Target: "external", Mode: string(api.ModeCI)})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	s := &Server{worktree: worktree, instanceID: inst.ID, projectName: name, subscribers: map[chan api.Event]bool{}}
	for _, req := range []Request{
		{Action: ActionWatch, Project: name, Target: "up"},
		{Action: ActionFlush, Project: name, Target: "up", TimeoutMs: 100},
		{Action: ActionRunAction, Project: name, ActionID: "change", Inputs: map[string]string{"value": "temporary"}},
		{Action: ActionStop, All: true},
	} {
		resp := s.handleRequest(context.Background(), req)
		if resp.OK || resp.Error == nil || resp.Error.Code != "resource_conflict" || resp.ResourceConflict == nil || resp.ResourceConflict.Target != "external" {
			t.Errorf("%s: expected structured ownership conflict, got %+v", req.Action, resp)
		}
		after, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Errorf("%s overwrote owner instance state", req.Action)
		}
	}
	status, err := instance.LoadStatus(worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Target != "external" || status.Nodes["check"].State != api.StateRunning {
		t.Fatalf("rejected commands changed owner status: %+v", status)
	}
	if !lease.ValidFor(worktree) {
		t.Fatal("stop-all released the external execution's ownership")
	}
	if _, err := os.Stat(instance.FlushRoot(worktree, inst.ID)); !os.IsNotExist(err) {
		t.Fatalf("rejected flush wrote coordination files: %v", err)
	}
}

func TestStopAllRecoveryRefusesUnconfirmedResources(t *testing.T) {
	for _, node := range []api.NodeStatus{
		{Name: "finite-handle", Kind: "once", State: api.StateDegraded, Generation: 1},
		{Name: "interrupted-command", Kind: "once", State: api.StateRunning},
	} {
		t.Run(node.Name, func(t *testing.T) {
			worktree := t.TempDir()
			inst, err := instance.Resolve(worktree, "test")
			if err != nil {
				t.Fatal(err)
			}
			if err := instance.SaveStatus(worktree, inst.ID, "up", api.ModeCI, map[string]api.NodeStatus{node.Name: node}); err != nil {
				t.Fatal(err)
			}
			lease, err := execution.Acquire(worktree, execution.Owner{Kind: "ci", Target: "up"})
			if err != nil {
				t.Fatal(err)
			}
			lease.RequireRecovery()
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
			s := &Server{worktree: worktree, instanceID: inst.ID}
			resp := s.handleRequest(context.Background(), Request{Action: ActionStop, All: true})
			if resp.OK || resp.Error == nil || resp.Error.Code != "resource_conflict" || resp.ResourceConflict == nil || !resp.ResourceConflict.RecoveryRequired {
				t.Fatalf("uncertain orphan resource was treated as stopped: %+v", resp)
			}
			owner, err := execution.ReadOwner(worktree)
			if err != nil || owner == nil {
				t.Fatalf("failed recovery discarded ownership record: owner=%+v err=%v", owner, err)
			}
			status, err := instance.LoadStatus(worktree, inst.ID)
			if err != nil {
				t.Fatal(err)
			}
			if status.Nodes[node.Name].State != node.State {
				t.Fatal("failed recovery marked unresolved resource stopped")
			}
		})
	}
}

func TestStopExistingDaemonFailurePreservesSocket(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("df-stop-refused-%d.sock", time.Now().UnixNano()))
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 2 {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
			var req Request
			if err := dec.Decode(&req); err != nil {
				conn.Close()
				return
			}
			resp := Response{ID: req.ID, OK: true}
			if req.Action == ActionStop {
				resp.OK = false
				resp.Error = &api.CommandError{Code: "resource_conflict", Phase: "admission", Message: "active execution did not stop"}
			}
			_ = enc.Encode(frame{Type: responseFrameType, ID: req.ID, Response: &resp})
			var ack frame
			_ = dec.Decode(&ack)
			conn.Close()
		}
	}()
	client := &Client{socketPath: socketPath}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := stopExistingDaemon(ctx, t.TempDir(), "unused", client); err == nil {
		t.Fatal("failed stop incorrectly allowed daemon replacement")
	}
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("failed replacement removed the live daemon socket: %v", err)
	}
	<-done
}

func TestStopAllWithoutMarkerPreservesUnknownResource(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	node := api.NodeStatus{Name: "interrupted", Kind: "once", State: api.StateRunning}
	if err := instance.SaveStatus(worktree, inst.ID, "up", api.ModeCI, map[string]api.NodeStatus{node.Name: node}); err != nil {
		t.Fatal(err)
	}
	s := &Server{worktree: worktree, instanceID: inst.ID}
	resp := s.handleRequest(context.Background(), Request{Action: ActionStop, All: true})
	if resp.OK || resp.Error == nil || resp.Error.Code != "resource_conflict" {
		t.Errorf("stop-all claimed unknown resource stopped: %+v", resp)
	}
	status, err := instance.LoadStatus(worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Nodes[node.Name].State != api.StateRunning {
		t.Fatal("stop-all erased unresolved running task evidence")
	}
}

func TestStopAllClearsConfirmedDeadResourceStates(t *testing.T) {
	for _, state := range []api.NodeState{api.StateStarting, api.StateRestarting, api.StateDegraded} {
		t.Run(string(state), func(t *testing.T) {
			worktree := t.TempDir()
			inst, err := instance.Resolve(worktree, "test")
			if err != nil {
				t.Fatal(err)
			}
			node := api.NodeStatus{Name: "terminated", Kind: "service", State: state, PID: 99999999, Generation: 1}
			if instance.ProcessAlive(node.PID) {
				t.Skip("test placeholder PID unexpectedly exists")
			}
			if err := instance.SaveStatus(worktree, inst.ID, "up", api.ModeWatch, map[string]api.NodeStatus{node.Name: node}); err != nil {
				t.Fatal(err)
			}
			s := &Server{worktree: worktree, instanceID: inst.ID}
			if _, err := s.stopWork(context.Background(), true, ""); err != nil {
				t.Fatal(err)
			}
			status, err := instance.LoadStatus(worktree, inst.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := status.Nodes[node.Name]; got.State != api.StateStopped || got.PID != 0 {
				t.Fatalf("confirmed absent resource retains conflicting state: %+v", got)
			}
		})
	}
}

func TestStopAllPreservesCompletedFailureWithoutRecoveryMarker(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	node := api.NodeStatus{Name: "exited", Kind: "service", State: api.StateFailed, Generation: 1, LastError: "service exited"}
	if err := instance.SaveStatus(worktree, inst.ID, "up", api.ModeWatch, map[string]api.NodeStatus{node.Name: node}); err != nil {
		t.Fatal(err)
	}
	s := &Server{worktree: worktree, instanceID: inst.ID}
	if _, err := s.stopWork(context.Background(), true, ""); err != nil {
		t.Fatalf("clean completion without a recovery marker blocked stop: %v", err)
	}
	status, err := instance.LoadStatus(worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := status.Nodes[node.Name]; got.State != api.StateFailed || got.LastError != node.LastError {
		t.Fatalf("stop erased completed failure evidence: %+v", got)
	}
}

type failedExitHandle struct{ *daemonLifecycleHandle }

func (h *failedExitHandle) Wait() error {
	_ = h.daemonLifecycleHandle.Wait()
	return errors.New("fixture service exit")
}

func TestConfirmedServiceExitAllowsLaterDaemonExecution(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	handles := make(chan *failedExitHandle, 2)
	p := daemonTestProject{name: "daemon-confirmed-exit-relaunch", tasks: []project.Task{{Name: "serve", Kind: project.KindService, Ready: func(context.Context, *project.Runtime) error { return nil }, Run: func(_ context.Context, rt *project.Runtime) error {
		handle := &failedExitHandle{newDaemonLifecycleHandle()}
		rt.RegisterServiceHandle(handle)
		handles <- handle
		return nil
	}}}, targets: []project.Target{{Name: "dev", RootTasks: []string{"serve"}}}}
	project.Register(p)
	s := &Server{worktree: worktree, instanceID: inst.ID, projectName: p.name, subscribers: map[chan api.Event]bool{}}
	if _, err := s.startActive(context.Background(), p.name, "dev", api.ModeWatch, 1); err != nil {
		t.Fatal(err)
	}
	defer s.stopActive(3 * time.Second)
	if !waitForDaemonCondition(3*time.Second, func() bool {
		status, err := instance.LoadStatus(worktree, inst.ID)
		return err == nil && status.Nodes["serve"].Ready
	}) {
		t.Fatal("service did not reach readiness")
	}
	handle := <-handles
	if err := handle.Stop(); err != nil {
		t.Fatal(err)
	}
	if !waitForDaemonCondition(3*time.Second, func() bool {
		status, err := instance.LoadStatus(worktree, inst.ID)
		return err == nil && status.Nodes["serve"].State == api.StateFailed
	}) {
		t.Fatal("watcher did not observe confirmed service failure")
	}
	if !s.stopActive(3 * time.Second) {
		t.Fatal("watcher did not stop")
	}
	if owner, err := execution.ReadOwner(worktree); err != nil || owner != nil {
		t.Fatalf("confirmed cleanup left recovery marker: owner=%+v err=%v", owner, err)
	}
	if _, err := s.startActive(context.Background(), p.name, "dev", api.ModeWatch, 1); err != nil {
		t.Fatalf("confirmed failed service blocked replacement execution: %v", err)
	}
	select {
	case <-handles:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement did not execute")
	}
	if _, err := s.stopWork(context.Background(), true, ""); err != nil {
		t.Fatal(err)
	}
}
