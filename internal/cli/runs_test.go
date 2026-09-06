package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

func retainedCLIRun(t *testing.T, state api.RunState) (string, *api.RunRecord) {
	t.Helper()
	wt := t.TempDir()
	// Retained evidence must remain readable even when the adapter no longer compiles.
	if err := os.WriteFile(filepath.Join(wt, "devflow.project.go"), []byte("deliberately broken adapter"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, _, err := instance.IDForWorktree(wt)
	if err != nil {
		t.Fatal(err)
	}
	record := &api.RunRecord{InstanceID: id, Project: "record-fixture", Target: "verify", Mode: api.ModeCI}
	if err := instance.CreateRun(wt, id, record); err != nil {
		t.Fatal(err)
	}
	record.State = state
	if state.Terminal() {
		record.FinishedAt = time.Now().UTC()
		record.Result = &api.RunResult{Target: "verify", Success: state == api.RunSucceeded}
	}
	if err := instance.SaveRun(wt, id, record); err != nil {
		t.Fatal(err)
	}
	return wt, record
}

func runEvidenceCLI(t *testing.T, input string, args ...string) (map[string]json.RawMessage, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := New()
	app.Stdout = &stdout
	app.Stderr = &stderr
	app.Stdin = strings.NewReader(input)
	err := app.Run(args)
	var result map[string]json.RawMessage
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatalf("expected one JSON document: %s (%v); command error %v", stdout.String(), decodeErr, err)
	}
	return result, stderr.String(), err
}

func TestRunsListAndShowWithoutLoadingAdapter(t *testing.T) {
	wt, record := retainedCLIRun(t, api.RunSucceeded)
	result, stderr, err := runEvidenceCLI(t, "", "runs", "list", "--worktree", wt, "--json")
	if err != nil || stderr != "" {
		t.Fatalf("list failed: %v stderr=%q", err, stderr)
	}
	var runs []struct {
		RunID  string       `json:"runId"`
		State  api.RunState `json:"state"`
		Target string       `json:"target"`
	}
	if err := json.Unmarshal(result["runs"], &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].RunID != record.RunID || runs[0].State != api.RunSucceeded || runs[0].Target != "verify" {
		t.Fatalf("wrong summaries: %+v", runs)
	}
	result, stderr, err = runEvidenceCLI(t, "", "runs", "show", record.RunID, "--json", "--worktree", wt)
	if err != nil || stderr != "" {
		t.Fatalf("show failed: %v stderr=%q", err, stderr)
	}
	var found api.RunRecord
	data, _ := json.Marshal(result)
	if err := json.Unmarshal(data, &found); err != nil {
		t.Fatal(err)
	}
	if found.RunID != record.RunID || found.Result == nil || !found.Result.Success {
		t.Fatalf("lost historical evidence: %+v", found)
	}
	if _, ok := result["prompts"]; !ok {
		t.Fatal("show omits reconnectable prompts")
	}
}

func TestRunsCancelAddressesOnlyRequestedRun(t *testing.T) {
	wt, record := retainedCLIRun(t, api.RunRunning)
	newer := &api.RunRecord{InstanceID: record.InstanceID, Project: "record-fixture", Target: "dev", Mode: api.ModeWatch}
	if err := instance.CreateRun(wt, record.InstanceID, newer); err != nil {
		t.Fatal(err)
	}
	result, stderr, err := runEvidenceCLI(t, "", "runs", "cancel", record.RunID, "--worktree", wt, "--json")
	if err != nil || stderr != "" {
		t.Fatalf("cancel failed: %v stderr=%q", err, stderr)
	}
	if string(result["accepted"]) != "true" {
		t.Fatalf("missing acceptance: %s", result["accepted"])
	}
	if err := instance.CheckRunCancellation(context.Background(), wt, record.InstanceID, record.RunID); !errors.Is(err, context.Canceled) {
		t.Fatalf("selected run not canceled: %v", err)
	}
	if err := instance.CheckRunCancellation(context.Background(), wt, record.InstanceID, newer.RunID); err != nil {
		t.Fatalf("unrelated run canceled: %v", err)
	}
}

func TestRunsErrorsKeepCurrentJSONContract(t *testing.T) {
	wt, record := retainedCLIRun(t, api.RunRunning)
	old := record.RunID
	record.State = api.RunSucceeded
	record.FinishedAt = time.Now().Add(-2 * time.Hour)
	if err := instance.SaveRun(wt, record.InstanceID, record); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.PruneRuns(wt, record.InstanceID, instance.RunRetention{MaxAge: time.Hour}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		code string
	}{
		{"expired", []string{"runs", "show", old}, "run_expired"},
		{"unknown", []string{"runs", "show", "run-" + record.InstanceID + "-ffffffffffffffff"}, "unknown_run"},
		{"malformed", []string{"runs", "show", "../../outside"}, "invalid_arguments"},
		{"missing ID", []string{"runs", "show"}, "invalid_arguments"},
		{"extra ID", []string{"runs", "show", old, "extra"}, "invalid_arguments"},
		{"unknown flag", []string{"runs", "list", "--unrecognized"}, "invalid_arguments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, stderr, err := runEvidenceCLI(t, "", append(tc.args, "--worktree", wt, "--json")...)
			if err == nil || stderr != "" {
				t.Fatalf("failure not machine clean: err=%v stderr=%q", err, stderr)
			}
			var commandErr api.CommandError
			if err := json.Unmarshal(result["error"], &commandErr); err != nil {
				t.Fatal(err)
			}
			if commandErr.Code != tc.code || commandErr.Phase == "" {
				t.Fatalf("wrong error: %+v", commandErr)
			}
		})
	}
}

func TestPromptCLIListsAndRespondsWithoutExposingSecret(t *testing.T) {
	wt, record := retainedCLIRun(t, api.RunWaiting)
	prompt, err := instance.CreatePrompt(context.Background(), wt, record.InstanceID, api.Prompt{RunID: record.RunID, Task: "migration", AttemptID: persistCLIPromptAttempt(t, wt, record, "migration"), Kind: "text", Secret: true, Message: "Database password"})
	if err != nil {
		t.Fatal(err)
	}
	result, stderr, err := runEvidenceCLI(t, "", "prompts", "list", "--run", record.RunID, "--worktree", wt, "--json")
	if err != nil || stderr != "" {
		t.Fatalf("list failed: %v %q", err, stderr)
	}
	var prompts []api.Prompt
	if err := json.Unmarshal(result["prompts"], &prompts); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || prompts[0].ID != prompt.ID || prompts[0].State != api.PromptPending {
		t.Fatalf("missing pending prompt: %+v", prompts)
	}
	secret := "private-answer-please-do-not-log"
	result, stderr, err = runEvidenceCLI(t, secret+"\n", "prompts", "respond", prompt.ID, "--run", record.RunID, "--task", prompt.Task, "--attempt", prompt.AttemptID, "--stdin", "--worktree", wt, "--json")
	if err != nil || stderr != "" {
		t.Fatalf("response failed: %v stderr=%q", err, stderr)
	}
	encoded, _ := json.Marshal(result)
	if bytes.Contains(encoded, []byte(secret)) || strings.Contains(stderr, secret) {
		t.Fatalf("secret leaked: %s stderr=%q", encoded, stderr)
	}
	answer, err := instance.ConsumePromptAnswer(context.Background(), wt, record.InstanceID, record.RunID, prompt.ID)
	if err != nil || answer == nil || answer.Text == nil || *answer.Text != secret {
		t.Fatalf("wrong answer: %+v %v", answer, err)
	}
}

func TestPromptCLIRejectsMissingAmbiguousAndWrongAnswers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		code  string
	}{
		{"missing answer", nil, "invalid_arguments"},
		{"ambiguous", []string{"--text", "yes", "--confirm", "true"}, "invalid_arguments"},
		{"invalid boolean", []string{"--confirm", "yes"}, "invalid_arguments"},
		{"wrong kind", []string{"--text", "true"}, "invalid_prompt_answer"},
		{"stdin and text", []string{"--stdin", "--text", "yes"}, "invalid_arguments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wt, record := retainedCLIRun(t, api.RunWaiting)
			prompt, err := instance.CreatePrompt(context.Background(), wt, record.InstanceID, api.Prompt{RunID: record.RunID, Task: "migration", AttemptID: persistCLIPromptAttempt(t, wt, record, "migration"), Kind: "confirm", Message: "Apply migration?"})
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"prompts", "respond", prompt.ID, "--run", record.RunID, "--task", prompt.Task, "--attempt", prompt.AttemptID, "--worktree", wt, "--json"}
			result, stderr, err := runEvidenceCLI(t, "", append(args, tc.flags...)...)
			if err == nil || stderr != "" {
				t.Fatalf("failure not machine clean: err=%v stderr=%q", err, stderr)
			}
			var commandErr api.CommandError
			if err := json.Unmarshal(result["error"], &commandErr); err != nil {
				t.Fatal(err)
			}
			if commandErr.Code != tc.code {
				t.Fatalf("wrong error: %+v", commandErr)
			}
		})
	}
}

func TestPromptCLIPreservesFalseEmptyAndLiteralFlagText(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		flags      []string
		want       string
	}{
		{"false", "confirm", []string{"--confirm", "false"}, "false"},
		{"empty", "text", []string{"--text="}, ""},
		{"literal JSON flag", "text", []string{"--text", "--json"}, "--json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wt, record := retainedCLIRun(t, api.RunWaiting)
			prompt, err := instance.CreatePrompt(context.Background(), wt, record.InstanceID, api.Prompt{RunID: record.RunID, Task: "task", AttemptID: persistCLIPromptAttempt(t, wt, record, "task"), Kind: tc.kind, Message: "Provide answer"})
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"prompts", "respond", prompt.ID, "--run", record.RunID, "--task", prompt.Task, "--attempt", prompt.AttemptID, "--worktree", wt, "--json"}
			_, stderr, err := runEvidenceCLI(t, "", append(args, tc.flags...)...)
			if err != nil || stderr != "" {
				t.Fatalf("answer failed: %v stderr=%q", err, stderr)
			}
			answer, err := instance.ConsumePromptAnswer(context.Background(), wt, record.InstanceID, record.RunID, prompt.ID)
			if err != nil || answer == nil {
				t.Fatalf("missing answer: %+v %v", answer, err)
			}
			if tc.kind == "confirm" {
				if answer.Confirm == nil || *answer.Confirm {
					t.Fatalf("false answer lost: %+v", answer)
				}
			} else if answer.Text == nil || *answer.Text != tc.want {
				t.Fatalf("text answer changed: %+v", answer)
			}
			result, _, err := runEvidenceCLI(t, "", append(args, tc.flags...)...)
			if err == nil {
				t.Fatal("duplicate answer accepted")
			}
			var detail api.CommandError
			if err := json.Unmarshal(result["error"], &detail); err != nil {
				t.Fatal(err)
			}
			if detail.Code != "prompt_not_pending" {
				t.Fatalf("wrong duplicate error: %+v", detail)
			}
		})
	}
}

func TestPromptStdinBoundsAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		fails             bool
	}{
		{"CRLF", "first\nsecond\r\n", "first\nsecond", false},
		{"one newline only", "answer\n\n", "answer\n", false},
		{"empty", "", "", false},
		{"oversized", strings.Repeat("x", (64<<10)+1), "", true},
		{"invalid UTF8", string([]byte{0xff}), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readPromptText(context.Background(), strings.NewReader(tc.input))
			if (err != nil) != tc.fails || got != tc.want {
				t.Fatalf("unexpected result length=%d, err=%v", len(got), err)
			}
		})
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := readPromptText(ctx, reader); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stdin kept canceled command blocked")
	}
}

func TestCompiledRunsRetrievalWithBrokenAdapter(t *testing.T) {
	wt, record := retainedCLIRun(t, api.RunSucceeded)
	stdout, stderr, err := runBootstrapCommandCaptured(t, wt, "runs", "show", record.RunID, "--json")
	if err != nil || stderr != "" {
		t.Fatalf("compiled retrieval bootstrapped broken adapter: %v stderr=%q stdout=%q", err, stderr, stdout)
	}
	var found api.RunRecord
	if err := json.Unmarshal([]byte(stdout), &found); err != nil {
		t.Fatal(err)
	}
	if found.RunID != record.RunID || found.Result == nil || !found.Result.Success {
		t.Fatalf("compiled retrieval lost evidence: %+v", found)
	}
	stdout, stderr, err = runBootstrapCommandCaptured(t, wt, "prompts", "list", "--run", record.RunID, "--json")
	if err != nil || stderr != "" {
		t.Fatalf("compiled prompts retrieval failed: %v stderr=%q stdout=%q", err, stderr, stdout)
	}
	var payload struct {
		RunID   string       `json:"runId"`
		Prompts []api.Prompt `json:"prompts"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RunID != record.RunID || payload.Prompts == nil {
		t.Fatalf("compiled prompts contract changed: %+v", payload)
	}
}

func TestRunsListWithoutStateIsReadOnly(t *testing.T) {
	wt := t.TempDir()
	result, stderr, err := runEvidenceCLI(t, "", "runs", "list", "--worktree", wt, "--json")
	if err != nil || stderr != "" || string(result["runs"]) != "[]" {
		t.Fatalf("empty list: %v stderr=%q runs=%s", err, stderr, result["runs"])
	}
	if _, err := os.Stat(filepath.Join(wt, ".devflow")); !os.IsNotExist(err) {
		t.Fatalf("read-only list created state: %v", err)
	}
}

func TestRunsReportsDeadOwnerWithoutFinalizingEvidence(t *testing.T) {
	wt, record := retainedCLIRun(t, api.RunRunning)
	record.OwnerPID = 1 << 30
	if err := instance.SaveRun(wt, record.InstanceID, record); err != nil {
		t.Fatal(err)
	}
	path, err := instance.RunPath(wt, record.InstanceID, record.RunID)
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(path, "record.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result, stderr, err := runEvidenceCLI(t, "", "runs", "show", record.RunID, "--worktree", wt, "--json")
	if err != nil || stderr != "" || string(result["ownerAlive"]) != "false" || string(result["state"]) != `"running"` {
		t.Fatalf("dead owner status lost or finalized: result=%v err=%v stderr=%q", result, err, stderr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only inspection finalized interrupted ownership")
	}
}

func persistCLIPromptAttempt(t *testing.T, worktree string, record *api.RunRecord, task string) string {
	t.Helper()
	attemptID := instance.NewAttemptID()
	record.Attempts = append(record.Attempts, api.TaskAttempt{Task: task, AttemptID: attemptID, State: api.StateRunning, StartedAt: time.Now().UTC()})
	if err := instance.SaveRun(worktree, record.InstanceID, record); err != nil {
		t.Fatal(err)
	}
	return attemptID
}
