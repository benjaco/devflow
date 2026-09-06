package tui

import (
	"context"
	"os"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

func TestDashboardRecoversAndQueuesPersistedPrompts(t *testing.T) {
	root, cache := t.TempDir(), t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	inst, err := instance.Resolve(root, "prompt-test")
	if err != nil {
		t.Fatal(err)
	}
	record := &api.RunRecord{Project: "prompt-test", Target: "verify", Mode: api.ModeCI, OwnerPID: os.Getpid()}
	if err := instance.CreateRun(root, inst.ID, record); err != nil {
		t.Fatal(err)
	}
	if err := instance.SaveStatus(root, inst.ID, "verify", api.ModeCI, map[string]api.NodeStatus{
		"check": {Name: "check", Kind: "once", RunID: record.RunID, State: api.StateRunning},
	}); err != nil {
		t.Fatal(err)
	}
	var prompts []api.Prompt
	for _, task := range []string{"left", "right"} {
		attemptID := instance.NewAttemptID()
		record.Attempts = append(record.Attempts, api.TaskAttempt{Task: task, AttemptID: attemptID, State: api.StateRunning})
		if err := instance.SaveRun(root, inst.ID, record); err != nil {
			t.Fatal(err)
		}
		prompt, err := instance.CreatePrompt(context.Background(), root, inst.ID, api.Prompt{
			RunID: record.RunID, Task: task, AttemptID: attemptID, Kind: "confirm", Message: "Continue?",
		})
		if err != nil {
			t.Fatal(err)
		}
		prompts = append(prompts, prompt)
	}
	d := newDashboard(root, inst.ID)
	if err := d.refresh(); err != nil {
		t.Fatal(err)
	}
	if d.activePromptID != prompts[0].ID {
		t.Fatalf("fresh observer did not restore first pending prompt: %q", d.activePromptID)
	}
	d.openPrompt(api.Event{PromptID: prompts[1].ID, PromptKind: "confirm", Prompt: "Continue?"})
	if d.activePromptID != prompts[0].ID {
		t.Fatal("parallel event replaced the active prompt")
	}
	yes := true
	if err := instance.RespondPrompt(context.Background(), root, inst.ID, api.PromptAnswer{
		RunID: prompts[0].RunID, Task: prompts[0].Task, AttemptID: prompts[0].AttemptID, PromptID: prompts[0].ID, Confirm: &yes,
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.refresh(); err != nil {
		t.Fatal(err)
	}
	if d.activePromptID != prompts[1].ID {
		t.Fatalf("second pending prompt was lost after answering first: %q", d.activePromptID)
	}
	if err := instance.ClosePrompt(context.Background(), root, inst.ID, record.RunID, prompts[1].ID, api.PromptCancelled); err != nil {
		t.Fatal(err)
	}
	if err := d.refresh(); err != nil {
		t.Fatal(err)
	}
	if d.activePromptID != "" || d.activeInput {
		t.Fatal("cancelled prompt dialog remained open")
	}
}
