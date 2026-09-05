package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func TestFlushWaitsForCapturedWatchObserver(t *testing.T) {
	s, active := newFlushGenerationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result, err := s.flush(ctx, active.projectName, active.target, 5*time.Second, 1)
	if !errors.Is(err, context.DeadlineExceeded) || len(result.Issues) != 1 || result.Issues[0].Kind != "timeout" {
		t.Errorf("observer wait ignored cancellation: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(instance.FlushRoot(s.worktree, s.instanceID)); !os.IsNotExist(err) {
		t.Errorf("flush published a sentinel before its watch captured a baseline: %v", err)
	}
}

func TestFlushRejectsReplacementWatchAcknowledgement(t *testing.T) {
	s, active := newFlushGenerationFixture(t)
	close(active.watchReady)
	finished := make(chan flushTestOutcome, 1)
	go func() {
		result, err := s.flush(context.Background(), active.projectName, active.target, 5*time.Second, 1)
		finished <- flushTestOutcome{result, err}
	}()
	request := waitForGenerationFlushRequest(t, s)
	s.transitionMu.Lock()
	s.mu.Lock()
	// The target is deliberately unchanged: target equality cannot identify a watch.
	s.active = &activeRun{projectName: active.projectName, target: active.target, mode: api.ModeWatch, done: make(chan struct{}), watchReady: make(chan struct{})}
	close(active.done)
	s.mu.Unlock()
	writeErr := instance.WriteFlushAck(s.worktree, s.instanceID, api.FlushResult{RequestID: request.ID, Target: active.target, Success: true, Synced: true})
	s.transitionMu.Unlock()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	outcome := <-finished
	if outcome.err == nil || outcome.result.Success || len(outcome.result.Issues) != 1 || outcome.result.Issues[0].Kind != "watch_stopped" {
		t.Fatalf("replacement watch acknowledgement was accepted: result=%+v err=%v", outcome.result, outcome.err)
	}
}

func TestFlushCancellationWhileWaitingForAck(t *testing.T) {
	s, active := newFlushGenerationFixture(t)
	close(active.watchReady)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan flushTestOutcome, 1)
	go func() {
		result, err := s.flush(ctx, active.projectName, active.target, 5*time.Second, 1)
		finished <- flushTestOutcome{result, err}
	}()
	_ = waitForGenerationFlushRequest(t, s)
	cancel()
	outcome := <-finished
	if !errors.Is(outcome.err, context.Canceled) || len(outcome.result.Issues) != 1 || outcome.result.Issues[0].Kind != "canceled" {
		t.Fatalf("ack wait ignored cancellation: result=%+v err=%v", outcome.result, outcome.err)
	}
}

func TestFlushRejectsWatchStoppedBeforeObserverReady(t *testing.T) {
	s, active := newFlushGenerationFixture(t)
	close(active.done)
	result, err := s.flush(context.Background(), active.projectName, active.target, time.Second, 1)
	if err == nil || len(result.Issues) != 1 || result.Issues[0].Kind != "watch_stopped" {
		t.Fatalf("stopped watch was accepted: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(instance.FlushRoot(s.worktree, s.instanceID)); !os.IsNotExist(err) {
		t.Errorf("stopped watch wrote coordination files: %v", err)
	}
}

func TestFlushPublicationRejectsReplacedWatch(t *testing.T) {
	s, active := newFlushGenerationFixture(t)
	close(active.watchReady)
	s.active = &activeRun{projectName: active.projectName, target: active.target, mode: api.ModeWatch}
	req := api.FlushRequest{ID: "replaced", SyncPath: instance.FlushSyncPath(s.worktree, s.instanceID, "replaced")}
	if _, err := s.publishFlushRequest(context.Background(), active, req); !errors.Is(err, errFlushWatchStopped) {
		t.Fatalf("request publication did not check captured watch: %v", err)
	}
	if _, err := os.Stat(instance.FlushRoot(s.worktree, s.instanceID)); !os.IsNotExist(err) {
		t.Errorf("replaced watch wrote coordination files: %v", err)
	}
}

func TestFlushRejectsDifferentProject(t *testing.T) {
	s, active := newFlushGenerationFixture(t)
	close(active.watchReady)
	other := active.projectName + "-other"
	project.Register(daemonTestProject{name: other, tasks: []project.Task{{Name: "noop", Kind: project.KindGroup}}, targets: []project.Target{{Name: active.target, RootTasks: []string{"noop"}}}})
	result, err := s.flush(context.Background(), other, active.target, 100*time.Millisecond, 1)
	if err == nil || len(result.Issues) != 1 || result.Issues[0].Kind != "project_mismatch" {
		t.Fatalf("flush did not bind the selected project: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(instance.FlushRoot(s.worktree, s.instanceID)); !os.IsNotExist(err) {
		t.Errorf("mismatched project wrote coordination files: %v", err)
	}
}

type flushTestOutcome struct {
	result api.FlushResult
	err    error
}

func newFlushGenerationFixture(t *testing.T) (*Server, *activeRun) {
	t.Helper()
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	name := t.Name()
	project.Register(daemonTestProject{name: name, tasks: []project.Task{{Name: "noop", Kind: project.KindGroup}}, targets: []project.Target{{Name: "up", RootTasks: []string{"noop"}}}})
	active := &activeRun{projectName: name, target: "up", mode: api.ModeWatch, done: make(chan struct{}), watchReady: make(chan struct{})}
	return &Server{worktree: worktree, instanceID: inst.ID, projectName: name, active: active}, active
}

func waitForGenerationFlushRequest(t *testing.T, s *Server) api.FlushRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		paths, err := filepath.Glob(filepath.Join(instance.FlushRoot(s.worktree, s.instanceID), "requests", "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			id := filepath.Base(path)
			request, err := instance.LoadFlushRequest(s.worktree, s.instanceID, id[:len(id)-len(".json")])
			if err == nil {
				return request
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("flush did not publish a request")
	return api.FlushRequest{}
}
