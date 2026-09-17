package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

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

func TestChoicePromptUsesRealTUIKeysAndReconnect(t *testing.T) {
	for _, cancelChoice := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelChoice), func(t *testing.T) {
			root := t.TempDir()
			record := &api.RunRecord{Project: "prompt-ui", Target: "up", Mode: api.ModeWatch, OwnerPID: os.Getpid()}
			if err := instance.CreateRun(root, "fixture", record); err != nil {
				t.Fatal(err)
			}
			attempt := instance.NewAttemptID()
			record.Attempts = []api.TaskAttempt{{Task: "app", AttemptID: attempt, State: api.StateRunning}}
			if err := instance.SaveRun(root, "fixture", record); err != nil {
				t.Fatal(err)
			}
			prompt, err := instance.CreatePrompt(context.Background(), root, "fixture", api.Prompt{RunID: record.RunID, Task: "app", AttemptID: attempt, Kind: "select", Message: "Is headline created or renamed?", Choices: []string{"+ headline create column", "~ title › headline rename column"}})
			if err != nil {
				t.Fatal(err)
			}
			d := newDashboard(root, "fixture")
			screen := newObservedSimulationScreen(80, 24)
			d.app.SetScreen(screen)
			done := make(chan error, 1)
			go func() { done <- runTUIApplication(d.app) }()
			t.Cleanup(func() {
				d.app.Stop()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("TUI did not stop")
				}
			})
			screen.waitForFrame(t)
			d.app.QueueUpdateDraw(func() {
				d.reconcilePrompts(snapshot{state: &instance.State{RunID: record.RunID}, prompts: []api.Prompt{prompt}})
			})
			text := dashboardState(t, d, func() string { return simulationScreenText(screen, 80, 24) })
			if !strings.Contains(text, "headline") || !strings.Contains(text, "rename column") {
				t.Fatalf("choice dialog omitted question/options:\n%s", text)
			}
			if cancelChoice {
				screen.postKey(t, tcell.KeyEscape, 0)
			} else {
				screen.postKey(t, tcell.KeyDown, 0)
				screen.postKey(t, tcell.KeyEnter, 0)
			}
			answer, err := instance.ConsumePromptAnswer(context.Background(), root, "fixture", record.RunID, prompt.ID)
			if err != nil || answer == nil {
				t.Fatalf("TUI did not submit: %+v %v", answer, err)
			}
			if cancelChoice && !answer.Cancel {
				t.Fatalf("Escape did not cancel: %+v", answer)
			}
			if !cancelChoice && (answer.Choice == nil || *answer.Choice != 1) {
				t.Fatalf("arrow selection not delivered: %+v", answer)
			}
			if dashboardState(t, d, func() bool { return d.activeInput }) {
				t.Fatal("prompt stayed open after response")
			}
			if screen.finalized.Load() {
				t.Fatal("prompt Escape exited the dashboard")
			}
		})
	}
}
