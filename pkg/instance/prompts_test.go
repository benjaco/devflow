package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

func TestPromptResponsesAreIsolatedAndClaimedOnce(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const instanceID = "test-instance"
	first := createTestPrompt(t, root, instanceID, "left", "confirm")
	second := createTestPrompt(t, root, instanceID, "right", "confirm")
	if first.ID == second.ID {
		t.Fatal("parallel tasks received the same prompt identity")
	}
	yes := true
	answer := api.PromptAnswer{RunID: first.RunID, Task: first.Task, AttemptID: first.AttemptID, PromptID: first.ID, Confirm: &yes}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			err := RespondPrompt(ctx, root, instanceID, answer)
			if err == nil {
				accepted.Add(1)
			} else {
				assertPromptError(t, err, "prompt_not_pending")
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d competing responses, want exactly one", accepted.Load())
	}
	got, err := ConsumePromptAnswer(ctx, root, instanceID, first.RunID, first.ID)
	if err != nil || got == nil || got.Confirm == nil || !*got.Confirm {
		t.Fatalf("consume first response = %+v, %v", got, err)
	}
	got, err = ConsumePromptAnswer(ctx, root, instanceID, second.RunID, second.ID)
	if err != nil || got != nil {
		t.Fatalf("response leaked to parallel prompt: %+v, %v", got, err)
	}
	got, err = ConsumePromptAnswer(ctx, root, instanceID, first.RunID, first.ID)
	if err != nil || got != nil {
		t.Fatalf("response consumed twice: %+v, %v", got, err)
	}
	items, err := ListPrompts(ctx, root, instanceID, first.RunID)
	if err != nil || len(items) != 2 {
		t.Fatalf("reconnected observer metadata = %+v, %v", items, err)
	}
}

func TestPromptRejectsInvalidAndStaleAnswers(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const instanceID = "test-instance"
	prompt := createTestPrompt(t, root, instanceID, "migration", "confirm")
	yes, text := true, "yes"
	valid := api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Confirm: &yes}
	for _, tc := range []struct {
		name   string
		mutate func(*api.PromptAnswer)
		code   string
	}{
		{"text_for_confirm", func(a *api.PromptAnswer) { a.Confirm = nil; a.Text = &text }, "invalid_prompt_answer"},
		{"ambiguous_type", func(a *api.PromptAnswer) { a.Text = &text }, "invalid_prompt_answer"},
		{"missing_answer", func(a *api.PromptAnswer) { a.Confirm = nil }, "invalid_prompt_answer"},
		{"wrong_task", func(a *api.PromptAnswer) { a.Task = "different" }, "prompt_mismatch"},
		{"old_attempt", func(a *api.PromptAnswer) { a.AttemptID = "attempt-previous" }, "prompt_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := valid
			tc.mutate(&answer)
			assertPromptError(t, RespondPrompt(ctx, root, instanceID, answer), tc.code)
		})
	}
	if err := ClosePrompt(ctx, root, instanceID, prompt.RunID, prompt.ID, api.PromptCancelled); err != nil {
		t.Fatal(err)
	}
	assertPromptError(t, RespondPrompt(ctx, root, instanceID, valid), "prompt_not_pending")
	retry := createTestPrompt(t, root, instanceID, "migration", "confirm")
	if retry.ID == prompt.ID {
		t.Fatal("retry reused cancelled prompt identity")
	}
	if got, err := ConsumePromptAnswer(ctx, root, instanceID, retry.RunID, retry.ID); err != nil || got != nil {
		t.Fatalf("stale answer reached retry: %+v, %v", got, err)
	}
}

func TestSecretPromptAnswerIsTransientAndPrivate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const instanceID = "test-instance"
	prompt, err := CreatePrompt(ctx, root, instanceID, api.Prompt{RunID: testPromptRunID(t, root, instanceID), Task: "login", AttemptID: testPromptAttemptID(t, root, instanceID, "login"), Kind: "text", Message: "Password", Secret: true})
	if err != nil {
		t.Fatal(err)
	}
	secret := "never-retain-this-secret"
	answer := api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Text: &secret}
	if err := RespondPrompt(ctx, root, instanceID, answer); err != nil {
		t.Fatal(err)
	}
	items, err := ListPrompts(ctx, root, instanceID, prompt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(items)
	if err != nil || strings.Contains(string(encoded), secret) {
		t.Fatalf("metadata leaked answer: %s, %v", encoded, err)
	}
	if _, err := ConsumePromptAnswer(ctx, root, instanceID, prompt.RunID, prompt.ID); err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), secret) {
			t.Errorf("secret retained in %s", path)
		}
		if runtime.GOOS != "windows" && strings.HasSuffix(path, ".json") {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0o077 != 0 {
				t.Errorf("prompt file is not owner-only: %s (%v)", path, info.Mode())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExpiredAndFailedPromptsCannotBeAnswered(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, state := range []api.PromptState{api.PromptPending, api.PromptCancelled} {
		prompt, err := CreatePrompt(ctx, root, "instance", api.Prompt{RunID: testPromptRunID(t, root, "instance"), Task: "check", AttemptID: testPromptAttemptID(t, root, "instance", "check"), Kind: "confirm", State: state, Deadline: time.Now().Add(-time.Minute).UTC()})
		if err != nil {
			t.Fatal(err)
		}
		yes := true
		err = RespondPrompt(ctx, root, "instance", api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Confirm: &yes})
		assertPromptError(t, err, "prompt_not_pending")
	}
}

func TestPromptCancellationRemovesUndeliveredAnswer(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	prompt := createTestPrompt(t, root, "instance", "login", "text")
	secret := "unconsumed-secret"
	if err := RespondPrompt(ctx, root, "instance", api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Text: &secret}); err != nil {
		t.Fatal(err)
	}
	if err := CloseRunPrompts(ctx, root, "instance", prompt.RunID, api.PromptCancelled); err != nil {
		t.Fatal(err)
	}
	if answer, err := ConsumePromptAnswer(ctx, root, "instance", prompt.RunID, prompt.ID); err != nil || answer != nil {
		t.Fatalf("cancelled run still has a consumable secret: %+v, %v", answer, err)
	}
}

func TestPromptRejectsResponseAsSoonAsRunCancellationIsRequested(t *testing.T) {
	root := t.TempDir()
	prompt := createTestPrompt(t, root, "instance", "task", "confirm")
	if err := RequestRunCancellation(context.Background(), root, "instance", prompt.RunID); err != nil {
		t.Fatal(err)
	}
	yes := true
	err := RespondPrompt(context.Background(), root, "instance", api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Confirm: &yes})
	assertPromptError(t, err, "prompt_not_pending")
}

func TestPromptRejectsResponseAfterOwnerExited(t *testing.T) {
	assertPromptRejectsInactiveOwner(t, exitedPromptOwnerPID(t))
}

func TestPromptRejectsResponseWithoutOwner(t *testing.T) {
	assertPromptRejectsInactiveOwner(t, 0)
}

func assertPromptRejectsInactiveOwner(t *testing.T, ownerPID int) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	prompt := createTestPrompt(t, root, "instance", "login", "text")
	record, err := LoadRun(root, "instance", prompt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	record.OwnerPID = ownerPID
	record.State = api.RunWaiting
	if err := SaveRun(root, "instance", record); err != nil {
		t.Fatal(err)
	}
	items, err := ListPrompts(ctx, root, "instance", prompt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].State != api.PromptCancelled {
		t.Errorf("inactive owner still exposes an answerable prompt: %+v", items)
	}
	secret := "orphaned-secret"
	err = RespondPrompt(ctx, root, "instance", api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Text: &secret})
	assertPromptError(t, err, "prompt_not_pending")
	path, err := promptPath(root, "instance", prompt.RunID, prompt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(promptAnswerPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("response without an active owner left transient answer data: %v", err)
	}
	_, err = CreatePrompt(ctx, root, "instance", api.Prompt{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, Kind: "text", Message: "Late prompt"})
	assertPromptError(t, err, "prompt_not_pending")
	retained, err := LoadRun(root, "instance", prompt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.State != api.RunWaiting || !retained.UpdatedAt.Equal(record.UpdatedAt) {
		t.Errorf("prompt inspection/admission changed interrupted run evidence: %+v", retained)
	}
}

func TestRunCancellationRemovesUndeliveredPromptAnswers(t *testing.T) {
	for _, name := range []string{"live_owner", "exited_owner", "already_requested"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			prompt, err := CreatePrompt(ctx, root, "instance", api.Prompt{RunID: testPromptRunID(t, root, "instance"), Task: "login", AttemptID: testPromptAttemptID(t, root, "instance", "login"), Kind: "text", Message: "Password", Secret: true})
			if err != nil {
				t.Fatal(err)
			}
			secret := "unconsumed-secret"
			if err := RespondPrompt(ctx, root, "instance", api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Text: &secret}); err != nil {
				t.Fatal(err)
			}
			record, err := LoadRun(root, "instance", prompt.RunID)
			if err != nil {
				t.Fatal(err)
			}
			record.OwnerPID = os.Getpid()
			if name == "exited_owner" {
				record.OwnerPID = exitedPromptOwnerPID(t)
			}
			record.State = api.RunWaiting
			if err := SaveRun(root, "instance", record); err != nil {
				t.Fatal(err)
			}
			if name == "already_requested" {
				marker, err := cancellationPath(root, "instance", prompt.RunID)
				if err != nil {
					t.Fatal(err)
				}
				// A previous requester may have stopped after writing its marker.
				if err := os.WriteFile(marker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := RequestRunCancellation(ctx, root, "instance", prompt.RunID); err != nil {
					t.Fatal(err)
				}
			}
			path, err := promptPath(root, "instance", prompt.RunID, prompt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(promptAnswerPath(path)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("explicit cancellation retained an undelivered answer: %v", err)
			}
			retained, err := LoadRun(root, "instance", prompt.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if retained.State != api.RunWaiting || !retained.UpdatedAt.Equal(record.UpdatedAt) || !retained.FinishedAt.IsZero() {
				t.Errorf("answer cleanup changed run/resource completion evidence: %+v", retained)
			}
			if err := CheckRunCancellation(ctx, root, "instance", prompt.RunID); !errors.Is(err, context.Canceled) {
				t.Fatalf("answer cleanup lost cancellation marker: %v", err)
			}
		})
	}
}

func exitedPromptOwnerPID(t *testing.T) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	owner := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	if output, err := owner.CombinedOutput(); err != nil {
		t.Fatalf("prompt owner fixture: %v\n%s", err, output)
	}
	pid := owner.Process.Pid
	if ProcessAlive(pid) {
		t.Fatal("completed prompt owner fixture still reports alive")
	}
	return pid
}

func TestPromptUnknownRunErrorsAndPathValidation(t *testing.T) {
	root := t.TempDir()
	prompt := createTestPrompt(t, root, "instance", "task", "confirm")
	missing := t.TempDir()
	if _, err := ListPrompts(context.Background(), missing, "instance", prompt.RunID); !errors.Is(err, ErrRunUnknown) {
		t.Fatalf("missing run error = %v", err)
	}
	if _, err := ListPrompts(context.Background(), missing, "instance", "../invalid"); !errors.Is(err, ErrInvalidRunID) {
		t.Fatalf("invalid run error = %v", err)
	}
	yes := true
	err := RespondPrompt(context.Background(), missing, "instance", api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Confirm: &yes})
	if !errors.Is(err, ErrRunUnknown) {
		t.Fatalf("response to missing run = %v", err)
	}
}

func TestPromptRejectsFinishedOrReplacedAttempt(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(fmt.Sprintf("replaced=%t", replaced), func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			prompt := createTestPrompt(t, root, "instance", "service", "text")
			record, err := LoadRun(root, "instance", prompt.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if replaced {
				// Even a stale active state cannot make an older attempt current.
				record.Attempts = append(record.Attempts, api.TaskAttempt{Task: prompt.Task, AttemptID: NewAttemptID(), State: api.StateRunning})
			} else {
				record.Attempts[len(record.Attempts)-1].State = api.StateStopped
			}
			if err := SaveRun(root, "instance", record); err != nil {
				t.Fatal(err)
			}
			items, err := ListPrompts(ctx, root, "instance", prompt.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0].State != api.PromptCancelled {
				t.Errorf("inactive attempt still exposes a pending prompt: %+v", items)
			}
			text := "stale-secret"
			err = RespondPrompt(ctx, root, "instance", api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Text: &text})
			assertPromptError(t, err, "prompt_not_pending")
			_, err = CreatePrompt(ctx, root, "instance", api.Prompt{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, Kind: "text", Message: "Late prompt"})
			assertPromptError(t, err, "prompt_not_pending")
			value, err := ConsumePromptAnswer(ctx, root, "instance", prompt.RunID, prompt.ID)
			assertPromptError(t, err, "prompt_not_pending")
			if value != nil {
				t.Errorf("stale attempt consumed response: %+v", value)
			}
		})
	}
}

func TestPromptConsumeDropsAnswerWhenAttemptStopsAfterAdmission(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	prompt := createTestPrompt(t, root, "instance", "service", "text")
	text := "undelivered-secret"
	if err := RespondPrompt(ctx, root, "instance", api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Text: &text}); err != nil {
		t.Fatal(err)
	}
	record, err := LoadRun(root, "instance", prompt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	record.Attempts[len(record.Attempts)-1].State = api.StateStopped
	if err := SaveRun(root, "instance", record); err != nil {
		t.Fatal(err)
	}
	answer, err := ConsumePromptAnswer(ctx, root, "instance", prompt.RunID, prompt.ID)
	assertPromptError(t, err, "prompt_not_pending")
	if answer != nil {
		t.Errorf("stopped attempt received a response: %+v", answer)
	}
	path, err := promptPath(root, "instance", prompt.RunID, prompt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(promptAnswerPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stopped attempt retained secret delivery file: %v", err)
	}
}

func TestPromptAcceptsFalseAndEmptyText(t *testing.T) {
	for _, kind := range []string{"confirm", "text"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			prompt := createTestPrompt(t, root, "instance", "task", kind)
			answer := api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID}
			if kind == "confirm" {
				value := false
				answer.Confirm = &value
			} else {
				value := ""
				answer.Text = &value
			}
			if err := RespondPrompt(context.Background(), root, "instance", answer); err != nil {
				t.Fatal(err)
			}
			got, err := ConsumePromptAnswer(context.Background(), root, "instance", prompt.RunID, prompt.ID)
			if err != nil || got == nil {
				t.Fatalf("missing explicit empty/false answer: %+v, %v", got, err)
			}
		})
	}
}

func TestPromptCrossProcessResponseClaim(t *testing.T) {
	if root := os.Getenv("DEVFLOW_PROMPT_TEST_ROOT"); root != "" {
		var answer api.PromptAnswer
		if err := json.Unmarshal([]byte(os.Getenv("DEVFLOW_PROMPT_TEST_ANSWER")), &answer); err != nil {
			t.Fatal(err)
		}
		if err := RespondPrompt(context.Background(), root, "instance", answer); err != nil {
			assertPromptError(t, err, "prompt_not_pending")
		} else {
			fmt.Println("response-accepted")
		}
		return
	}
	root := t.TempDir()
	prompt := createTestPrompt(t, root, "instance", "task", "confirm")
	yes := true
	data, err := json.Marshal(api.PromptAnswer{RunID: prompt.RunID, Task: prompt.Task, AttemptID: prompt.AttemptID, PromptID: prompt.ID, Confirm: &yes})
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPromptCrossProcessResponseClaim$")
			cmd.Env = append(os.Environ(), "DEVFLOW_PROMPT_TEST_ROOT="+root, "DEVFLOW_PROMPT_TEST_ANSWER="+string(data))
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("responder failed: %v\n%s", err, output)
				return
			}
			if strings.Contains(string(output), "response-accepted") {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("%d separate processes claimed the prompt, want one", accepted.Load())
	}
}

func createTestPrompt(t *testing.T, root, instanceID, task, kind string) api.Prompt {
	t.Helper()
	prompt, err := CreatePrompt(context.Background(), root, instanceID, api.Prompt{RunID: testPromptRunID(t, root, instanceID), Task: task, AttemptID: testPromptAttemptID(t, root, instanceID, task), Kind: kind, Message: "Continue?"})
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}

func assertPromptError(t *testing.T, err error, code string) {
	t.Helper()
	var detail *api.CommandError
	if !errors.As(err, &detail) || detail.Code != code {
		t.Errorf("error = %v, want code %s", err, code)
	}
}

func testPromptRunID(t *testing.T, root, instanceID string) string {
	t.Helper()
	runs, err := ListRuns(root, instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) > 0 {
		return runs[0].RunID
	}
	record := &api.RunRecord{Project: "prompt-test", Target: "verify", Mode: api.ModeCI, State: api.RunRunning, OwnerPID: os.Getpid()}
	if err := CreateRun(root, instanceID, record); err != nil {
		t.Fatal(err)
	}
	return record.RunID
}

func testPromptAttemptID(t *testing.T, root, instanceID, task string) string {
	t.Helper()
	record, err := LoadRun(root, instanceID, testPromptRunID(t, root, instanceID))
	if err != nil {
		t.Fatal(err)
	}
	attemptID := NewAttemptID()
	record.State = api.RunRunning
	record.Attempts = append(record.Attempts, api.TaskAttempt{Task: task, AttemptID: attemptID, State: api.StateRunning})
	if err := SaveRun(root, instanceID, record); err != nil {
		t.Fatal(err)
	}
	return attemptID
}
