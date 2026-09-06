package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

type parallelPromptProject struct{}

func (parallelPromptProject) Name() string { return "parallel-prompts" }
func (parallelPromptProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{}, nil
}
func (parallelPromptProject) Targets() []project.Target {
	return []project.Target{{Name: "verify", RootTasks: []string{"left", "right"}}}
}
func (parallelPromptProject) Tasks() []project.Task {
	var tasks []project.Task
	for _, name := range []string{"left", "right"} {
		tasks = append(tasks, project.Task{Name: name, Kind: project.KindOnce, Run: func(_ context.Context, rt *project.Runtime) error {
			_, err := rt.OnPrompt(rt.TaskName, process.PromptRequest{Kind: process.PromptConfirm, Prompt: "Continue?"})
			return err
		}})
	}
	return tasks
}

func TestParallelTaskPromptsHaveDistinctIdentity(t *testing.T) {
	root, cache := t.TempDir(), t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	eng, err := New(parallelPromptProject{}, root)
	if err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEvents()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = eng.Run(ctx, Request{Worktree: root, Target: "verify", Mode: api.ModeCI, MaxParallel: 2, Headless: api.HeadlessWait})
	}()
	defer func() { cancel(); <-done }()
	var prompts []api.Event
	for len(prompts) < 2 {
		select {
		case evt := <-events:
			if evt.Type == api.EventInteractionReq {
				prompts = append(prompts, evt)
			}
		case <-ctx.Done():
			t.Fatal("parallel prompts were not published before timeout")
		}
	}
	if prompts[0].PromptID == prompts[1].PromptID {
		t.Fatalf("different tasks %s and %s share prompt identity %q", prompts[0].Task, prompts[1].Task, prompts[0].PromptID)
	}
	if prompts[0].RunID == "" || prompts[0].RunID != prompts[1].RunID || prompts[0].AttemptID == "" || prompts[0].AttemptID == prompts[1].AttemptID {
		t.Fatalf("parallel prompts are not bound to distinct attempts in one run: %+v", prompts)
	}
	persisted, err := instance.ListPrompts(context.Background(), root, prompts[0].InstanceID, prompts[0].RunID)
	if err != nil || len(persisted) != 2 {
		t.Fatalf("observer cannot reconnect to both pending prompts: %+v, %v", persisted, err)
	}
	record, err := instance.LoadRun(root, prompts[0].InstanceID, prompts[0].RunID)
	if err != nil || record.State != api.RunWaiting {
		t.Fatalf("pending operation is not inspectably waiting: %+v, %v", record, err)
	}
	cancel()
	<-done
	persisted, err = instance.ListPrompts(context.Background(), root, prompts[0].InstanceID, prompts[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range persisted {
		if prompt.State != api.PromptCancelled {
			t.Errorf("cancelled operation retained answerable prompt: %+v", prompt)
		}
	}
}

func TestHeadlessInteractiveProcessFailureDoesNotWaitForDeadline(t *testing.T) {
	root, cache := t.TempDir(), t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	// Keep fixture compilation outside the deadline for the process behavior.
	_ = buildPromptCLIForEngine(tRepoRoot())
	eng, err := New(interactiveProject{}, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := eng.Run(ctx, Request{Worktree: root, Target: "build", Mode: api.ModeCI})
	var detail *api.CommandError
	if !errors.As(err, &detail) || detail.Code != "interaction_required" {
		t.Fatalf("interactive subprocess lost headless failure: outcome=%+v error=%v", out, err)
	}
	if ctx.Err() != nil {
		t.Fatalf("interactive subprocess remained blocked until deadline: %v", ctx.Err())
	}
}

func TestWaitingPromptHonorsOperationDeadline(t *testing.T) {
	root, cache := t.TempDir(), t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	eng, err := New(parallelPromptProject{}, root)
	if err != nil {
		t.Fatal(err)
	}
	out, err := eng.Run(context.Background(), Request{Worktree: root, Target: "verify", Mode: api.ModeCI, Headless: api.HeadlessWait, Timeout: time.Second, MaxParallel: 2})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("execution did not honor its deadline: %v", err)
	}
	if out.Instance == nil {
		t.Fatalf("deadline lost instance evidence: %+v", out)
	}
	prompts, err := instance.ListPrompts(context.Background(), root, out.Instance.ID, out.Result.RunID)
	if err != nil || len(prompts) == 0 {
		t.Fatalf("deadline lost prompt evidence: %+v, %v", prompts, err)
	}
	for _, prompt := range prompts {
		if prompt.State != api.PromptExpired {
			t.Errorf("timed-out prompt did not expire: %+v", prompt)
		}
	}
}

func TestHeadlessPromptFailureClosesDiagnostic(t *testing.T) {
	root, cache := t.TempDir(), t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	eng, err := New(parallelPromptProject{}, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := eng.Run(ctx, Request{Worktree: root, Target: "verify", Mode: api.ModeCI, MaxParallel: 2})
	var detail *api.CommandError
	if !errors.As(err, &detail) || detail.Code != "interaction_required" {
		t.Fatalf("headless default did not report interaction_required: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("headless failure waited for deadline: %v", ctx.Err())
	}
	if out.Instance == nil || out.Result.RunID == "" {
		t.Fatalf("failure lost run evidence: %+v", out)
	}
	prompts, err := instance.ListPrompts(context.Background(), root, out.Instance.ID, out.Result.RunID)
	if err != nil || len(prompts) == 0 {
		t.Fatalf("missing diagnostic prompt: %+v, %v", prompts, err)
	}
	for _, prompt := range prompts {
		if prompt.State != api.PromptCancelled {
			t.Errorf("failed headless prompt remained answerable: %+v", prompt)
		}
		yes := true
		err := instance.RespondPrompt(context.Background(), root, out.Instance.ID, api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Confirm: &yes})
		if !errors.As(err, &detail) || detail.Code != "prompt_not_pending" {
			t.Errorf("failed diagnostic accepted a late answer: %v", err)
		}
	}
}
