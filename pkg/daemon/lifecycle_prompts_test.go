package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

func TestLifecycleReplacementPreservesExplicitPromptPolicy(t *testing.T) {
	for _, action := range []Action{ActionRetarget, ActionInvalidate, ActionRestart} {
		t.Run(string(action), func(t *testing.T) {
			worktree := t.TempDir()
			inst, err := instance.Resolve(worktree, "test")
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			name := "daemon-lifecycle-prompts-" + string(action)
			project.Register(daemonTestProject{name: name,
				tasks: []project.Task{{Name: "check", Kind: project.KindOnce, Run: func(_ context.Context, rt *project.Runtime) error {
					if calls.Add(1) == 1 {
						return nil
					}
					answer, err := rt.OnPrompt(rt.TaskName, process.PromptRequest{Kind: process.PromptConfirm, Prompt: "Continue?"})
					if err == nil && answer.Value != "y" {
						return errors.New("expected confirmation")
					}
					return err
				}}},
				targets: []project.Target{{Name: "up", RootTasks: []string{"check"}}, {Name: "other", RootTasks: []string{"check"}}},
			})
			s := &Server{worktree: worktree, instanceID: inst.ID, projectName: name, subscribers: map[chan api.Event]bool{}}
			t.Cleanup(func() { s.stopActive(3 * time.Second) })
			response := s.handleRequest(context.Background(), Request{Action: ActionWatch, Target: "up"})
			if !response.OK {
				t.Fatal(response.Error)
			}
			if !waitForDaemonCondition(3*time.Second, func() bool {
				state, err := instance.LoadStatus(worktree, inst.ID)
				return err == nil && state.Nodes["check"].State == api.StateDone
			}) {
				t.Fatal("initial watch did not settle")
			}
			finished := make(chan Response, 1)
			go func() {
				finished <- s.handleRequest(context.Background(), Request{Action: action, Target: "other", Task: "check", Headless: api.HeadlessWait})
			}()
			var prompt api.Prompt
			if !waitForDaemonCondition(2*time.Second, func() bool {
				status, err := s.statusResult()
				if err == nil && len(status.PendingPrompts) > 0 {
					prompt = status.PendingPrompts[0]
					return true
				}
				return false
			}) {
				t.Fatal("replacement discarded the explicit wait policy")
			}
			yes := true
			if err := instance.RespondPrompt(context.Background(), worktree, inst.ID, api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Confirm: &yes}); err != nil {
				t.Fatal(err)
			}
			select {
			case response := <-finished:
				if !response.OK {
					t.Fatal(response.Error)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("lifecycle operation did not return after its answer")
			}
		})
	}
}
