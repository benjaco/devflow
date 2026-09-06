package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func TestQueuedLifecycleDeadlinePreservesCurrentWatcher(t *testing.T) {
	for _, action := range []Action{ActionInvalidate, ActionRestart, ActionRetarget} {
		t.Run(string(action), func(t *testing.T) {
			s := runControlServer(t, "queued-lifecycle-deadline-"+string(action), func(context.Context, *project.Runtime) error { return nil })
			response := s.handleRequest(context.Background(), Request{Action: ActionWatch, Target: "check"})
			if !response.OK {
				t.Fatalf("start watcher: %+v", response)
			}
			if !waitForDaemonCondition(3*time.Second, func() bool {
				state, err := instance.LoadStatus(s.worktree, s.instanceID)
				return err == nil && state.Nodes["check"].State == api.StateDone
			}) {
				t.Fatal("watcher did not settle")
			}
			s.mu.Lock()
			before := s.active
			s.mu.Unlock()
			s.transitionMu.Lock()
			unlock := sync.OnceFunc(s.transitionMu.Unlock)
			defer unlock()
			finished := make(chan Response, 1)
			go func() {
				finished <- s.handleRequest(context.Background(), Request{Action: action, Task: "check", Target: "check", TimeoutMs: 50})
			}()
			select {
			case response = <-finished:
			case <-time.After(time.Second):
				t.Error("expired lifecycle request waited for mutation admission")
				unlock()
				select {
				case response = <-finished:
				case <-time.After(3 * time.Second):
					t.Fatal("expired lifecycle request did not finish")
				}
			}
			unlock()
			if response.OK || response.Error == nil || response.Error.Code != "deadline_exceeded" {
				t.Errorf("expired lifecycle response = %+v", response)
			}
			s.mu.Lock()
			after := s.active
			s.mu.Unlock()
			if before != after {
				t.Fatal("expired queued lifecycle request stopped the current watcher")
			}
			select {
			case <-before.done:
				t.Fatal("expired queued lifecycle request canceled the current watcher")
			default:
			}
		})
	}
}
