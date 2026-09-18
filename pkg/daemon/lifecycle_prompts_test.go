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

func TestActionRelaunchPreservesWatchPromptPolicy(t *testing.T) {
	for _, policy := range []api.HeadlessPolicy{api.HeadlessWait, api.HeadlessFail} {
		t.Run(string(policy), func(t *testing.T) {
			worktree := t.TempDir()
			inst, err := instance.Resolve(worktree, "test")
			if err != nil {
				t.Fatal(err)
			}
			var starts atomic.Int32
			promptErrors := make(chan error, 1)
			name := "action-relaunch-prompts-" + string(policy)
			project.Register(project.Define(func(_ context.Context, b *project.Builder) error {
				b.Name(name)
				check := b.Task("check").NoCache().Run(func(_ context.Context, rt *project.Runtime) error {
					if starts.Add(1) == 1 {
						return nil
					}
					answer, err := rt.OnPrompt(rt.TaskName, process.PromptRequest{
						Kind: process.PromptSelect, Prompt: "Create or rename?", Choices: []string{"Create column", "Rename column"},
					})
					promptErrors <- err
					if err == nil && answer.Value != "1" {
						return errors.New("expected rename selection")
					}
					return err
				})
				author := b.Task("author").NoCache().Run(func(context.Context, *project.Runtime) error { return nil })
				b.Target("up", check)
				b.Action("create").Task(author).RelaunchPreviousTargetAfterSuccess()
				return nil
			}))
			s := &Server{worktree: worktree, instanceID: inst.ID, projectName: name, subscribers: map[chan api.Event]bool{}}
			t.Cleanup(func() { s.stopActive(3 * time.Second) })
			response := s.handleRequest(context.Background(), Request{Action: ActionWatch, Target: "up", Headless: policy})
			if !response.OK {
				t.Fatal(response.Error)
			}
			if !waitForDaemonCondition(3*time.Second, func() bool {
				state, err := instance.LoadStatus(worktree, inst.ID)
				return err == nil && state.Nodes["check"].State == api.StateDone
			}) {
				t.Fatal("initial watcher did not settle")
			}
			// The foreground action's policy, deadline and cancellation are
			// independent of the development watcher it temporarily replaces.
			actionPolicy := api.HeadlessWait
			if policy == api.HeadlessWait {
				actionPolicy = api.HeadlessFail
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			response = s.handleRequest(ctx, Request{Action: ActionRunAction, ActionID: "create", Headless: actionPolicy, TimeoutMs: 5000})
			cancel()
			if !response.OK {
				t.Fatal(response.Error)
			}
			s.mu.Lock()
			resumed := s.active
			s.mu.Unlock()
			if resumed == nil || resumed.target != "up" || resumed.headless != policy {
				t.Fatalf("action lost the watch prompt policy: %+v", resumed)
			}
			record, err := instance.LoadRun(worktree, inst.ID, resumed.runID)
			if err != nil || !record.Deadline.IsZero() {
				t.Fatalf("action deadline leaked into resumed watch: %+v, %v", record, err)
			}
			if policy == api.HeadlessWait {
				var prompt api.Prompt
				if !waitForDaemonCondition(3*time.Second, func() bool {
					status, err := s.statusResult()
					if err == nil && len(status.PendingPrompts) == 1 {
						prompt = status.PendingPrompts[0]
						return true
					}
					return false
				}) {
					t.Fatal("resumed watch did not retain a pending prompt")
				}
				if prompt.RunID != resumed.runID || prompt.Kind != "select" || len(prompt.Choices) != 2 {
					t.Fatalf("incorrect resumed prompt identity: %+v", prompt)
				}
				choice := 1
				if err := instance.RespondPrompt(context.Background(), worktree, inst.ID, api.PromptAnswer{
					RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Choice: &choice,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if !waitForDaemonCondition(3*time.Second, func() bool {
				state, err := instance.LoadStatus(worktree, inst.ID)
				if err != nil {
					return false
				}
				node := state.Nodes["check"]
				if policy == api.HeadlessWait {
					return node.State == api.StateDone
				}
				return node.State == api.StateFailed
			}) {
				t.Fatal("resumed watch did not honor its own prompt policy")
			}
			if err := <-promptErrors; policy == api.HeadlessFail {
				var detail *api.CommandError
				if !errors.As(err, &detail) || detail.Code != "interaction_required" {
					t.Fatalf("headless failure lost its structured cause: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

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
					if err != nil {
						return err
					}
					answer, err = rt.OnPrompt(rt.TaskName, process.PromptRequest{Kind: process.PromptSelect, Prompt: "Create or rename?", Choices: []string{"Create column", "Rename column"}})
					if err == nil && answer.Value != "1" {
						return errors.New("expected rename selection")
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
			for _, kind := range []string{"confirm", "select"} {
				var prompt api.Prompt
				if !waitForDaemonCondition(2*time.Second, func() bool {
					status, err := s.statusResult()
					if err == nil && len(status.PendingPrompts) > 0 && status.PendingPrompts[0].Kind == kind {
						prompt = status.PendingPrompts[0]
						return true
					}
					return false
				}) {
					t.Fatalf("replacement discarded the explicit wait policy for %s", kind)
				}
				answer := api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID}
				if kind == "confirm" {
					yes := true
					answer.Confirm = &yes
				} else {
					if len(prompt.Choices) != 2 || prompt.Choices[1] != "Rename column" {
						t.Fatalf("daemon lost choice metadata: %+v", prompt)
					}
					index := 1
					answer.Choice = &index
				}
				if err := instance.RespondPrompt(context.Background(), worktree, inst.ID, answer); err != nil {
					t.Fatal(err)
				}
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
