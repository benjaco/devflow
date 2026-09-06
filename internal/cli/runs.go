package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benjaco/devflow/internal/clierror"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

type runSummary struct {
	RunID      string            `json:"runId"`
	Project    string            `json:"project"`
	Target     string            `json:"target"`
	Mode       api.RunMode       `json:"mode"`
	OwnerAlive *bool             `json:"ownerAlive,omitempty"`
	State      api.RunState      `json:"state"`
	CreatedAt  time.Time         `json:"createdAt"`
	StartedAt  time.Time         `json:"startedAt,omitempty"`
	FinishedAt time.Time         `json:"finishedAt,omitempty"`
	Error      *api.CommandError `json:"error,omitempty"`
}

func (a *App) runsCmd(args []string) error {
	if len(args) == 0 {
		return evidenceArguments("usage: devflow runs list|show|cancel")
	}
	action := args[0]
	if action != "list" && action != "show" && action != "cancel" {
		return evidenceArguments("unknown runs command %q", action)
	}
	fs := flag.NewFlagSet("runs "+action, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	worktree := fs.String("worktree", "", "")
	instanceID := fs.String("instance", "", "")
	if err := a.parseFlags(fs, args[1:]); err != nil {
		return err
	}
	if action != "list" && fs.NArg() != 1 {
		return evidenceArguments("runs %s requires a run ID", action)
	}
	root, id, err := resolveInstance(*worktree, *instanceID)
	if err != nil {
		return err
	}
	switch action {
	case "list":
		records, err := instance.ListRuns(root, id)
		if err != nil {
			return runEvidenceError(err)
		}
		summaries := make([]runSummary, 0, len(records))
		for _, record := range records {
			summary := runSummary{RunID: record.RunID, Project: record.Project, Target: record.Target, Mode: record.Mode, State: record.State, CreatedAt: record.CreatedAt, StartedAt: record.StartedAt, FinishedAt: record.FinishedAt}
			summary.OwnerAlive = runOwnerAlive(record)
			if record.Result != nil {
				summary.Error = record.Result.Error
			}
			summaries = append(summaries, summary)
		}
		if *jsonOut {
			return a.writeResult(struct {
				InstanceID string       `json:"instanceId"`
				Runs       []runSummary `json:"runs"`
			}{id, summaries})
		}
		for _, run := range summaries {
			if _, err := fmt.Fprintf(a.Stdout, "%s  %-10s %s  %s\n", run.RunID, run.State, run.Target, run.Mode); err != nil {
				return err
			}
		}
		return nil
	case "show":
		record, err := instance.LoadRun(root, id, fs.Arg(0))
		if err != nil {
			return runEvidenceError(err)
		}
		prompts, err := instance.ListPrompts(a.context(), root, id, record.RunID)
		if err != nil {
			return runEvidenceError(err)
		}
		if *jsonOut {
			return a.writeResult(struct {
				*api.RunRecord
				Prompts    []api.Prompt `json:"prompts"`
				OwnerAlive *bool        `json:"ownerAlive,omitempty"`
			}{record, prompts, runOwnerAlive(*record)})
		}
		if _, err := fmt.Fprintf(a.Stdout, "%s  %s\nproject: %s  target: %s  mode: %s\n", record.RunID, record.State, record.Project, record.Target, record.Mode); err != nil {
			return err
		}
		for _, attempt := range record.Attempts {
			if _, err := fmt.Fprintf(a.Stdout, "%s  %-10s %s  %s\n", attempt.AttemptID, attempt.State, attempt.Task, attempt.LogPath); err != nil {
				return err
			}
		}
		return nil
	case "cancel":
		runID := fs.Arg(0)
		if err := instance.RequestRunCancellation(a.context(), root, id, runID); err != nil {
			return runEvidenceError(err)
		}
		// Acceptance records the request. Cleanup and terminal evidence belong to
		// the execution owner, which can outlive this observing CLI process.
		if *jsonOut {
			return a.writeResult(struct {
				InstanceID string `json:"instanceId"`
				RunID      string `json:"runId"`
				Accepted   bool   `json:"accepted"`
			}{id, runID, true})
		}
		_, err := fmt.Fprintf(a.Stdout, "cancellation requested: %s\n", runID)
		return err
	}
	return nil
}

func (a *App) promptsCmd(args []string) error {
	if len(args) == 0 {
		return evidenceArguments("usage: devflow prompts list|respond")
	}
	action := args[0]
	if action != "list" && action != "respond" {
		return evidenceArguments("unknown prompts command %q", action)
	}
	fs := flag.NewFlagSet("prompts "+action, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	worktree := fs.String("worktree", "", "")
	instanceID := fs.String("instance", "", "")
	runID := fs.String("run", "", "")
	var task, attemptID, confirm, answerText string
	var fromStdin bool
	if action == "respond" {
		fs.StringVar(&task, "task", "", "")
		fs.StringVar(&attemptID, "attempt", "", "")
		fs.StringVar(&confirm, "confirm", "", "")
		fs.StringVar(&answerText, "text", "", "")
		fs.BoolVar(&fromStdin, "stdin", false, "")
	}
	if err := a.parseFlags(fs, args[1:]); err != nil {
		return err
	}
	if *runID == "" {
		return evidenceArguments("prompts %s requires --run", action)
	}
	if action == "respond" && (fs.NArg() != 1 || task == "" || attemptID == "") {
		return evidenceArguments("prompts respond requires a prompt ID, --task and --attempt")
	}
	root, id, err := resolveInstance(*worktree, *instanceID)
	if err != nil {
		return err
	}
	if action == "list" {
		prompts, err := instance.ListPrompts(a.context(), root, id, *runID)
		if err != nil {
			return runEvidenceError(err)
		}
		if *jsonOut {
			return a.writeResult(struct {
				InstanceID string       `json:"instanceId"`
				RunID      string       `json:"runId"`
				Prompts    []api.Prompt `json:"prompts"`
			}{id, *runID, prompts})
		}
		for _, prompt := range prompts {
			if _, err := fmt.Fprintf(a.Stdout, "%s  %-10s %s  %s: %s\n", prompt.ID, prompt.State, prompt.Task, prompt.Kind, prompt.Message); err != nil {
				return err
			}
		}
		return nil
	}
	answer := api.PromptAnswer{RunID: *runID, Task: task, AttemptID: attemptID, PromptID: fs.Arg(0)}
	var confirmSet, textSet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "confirm":
			confirmSet = true
		case "text":
			textSet = true
		}
	})
	count := 0
	for _, selected := range []bool{confirmSet, textSet, fromStdin} {
		if selected {
			count++
		}
	}
	if count != 1 {
		return evidenceArguments("provide exactly one of --confirm, --text or --stdin")
	}
	switch {
	case confirmSet:
		if confirm != "true" && confirm != "false" {
			return evidenceArguments("--confirm must be true or false")
		}
		value := confirm == "true"
		answer.Confirm = &value
	case textSet:
		answer.Text = &answerText
	case fromStdin:
		input := a.Stdin
		if input == nil {
			input = os.Stdin
		}
		value, err := readPromptText(a.context(), input)
		if err != nil {
			return err
		}
		answer.Text = &value
	}
	if err := instance.RespondPrompt(a.context(), root, id, answer); err != nil {
		return runEvidenceError(err)
	}
	if *jsonOut {
		return a.writeResult(struct {
			InstanceID string `json:"instanceId"`
			RunID      string `json:"runId"`
			Task       string `json:"task"`
			AttemptID  string `json:"attemptId"`
			PromptID   string `json:"promptId"`
			Accepted   bool   `json:"accepted"`
		}{id, answer.RunID, answer.Task, answer.AttemptID, answer.PromptID, true})
	}
	_, err = fmt.Fprintf(a.Stdout, "response accepted: %s\n", answer.PromptID)
	return err
}

func readPromptText(ctx context.Context, input io.Reader) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	type readResult struct {
		data []byte
		err  error
	}
	result := make(chan readResult, 1)
	// A pipe may not reach EOF; cancellation must still release the CLI waiter.
	go func() {
		data, err := io.ReadAll(io.LimitReader(input, (64<<10)+1))
		result <- readResult{data, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case read := <-result:
		if read.err != nil {
			return "", read.err
		}
		if len(read.data) > 64<<10 {
			return "", evidenceArguments("stdin answer exceeds 64 KiB")
		}
		if !utf8.Valid(read.data) {
			return "", evidenceArguments("stdin answer must be valid UTF-8")
		}
		value := strings.TrimSuffix(string(read.data), "\n")
		if len(value) != len(read.data) {
			value = strings.TrimSuffix(value, "\r")
		}
		return value, nil
	}
}

func evidenceArguments(format string, args ...any) error {
	return clierror.Wrap(fmt.Errorf(format, args...), "invalid_arguments", "parsing")
}

func runEvidenceError(err error) error {
	switch {
	case errors.Is(err, instance.ErrInvalidRunID), errors.Is(err, instance.ErrInvalidAttemptID):
		return clierror.Wrap(err, "invalid_arguments", "parsing")
	case errors.Is(err, instance.ErrRunUnknown):
		return clierror.Wrap(err, "unknown_run", "resolution")
	case errors.Is(err, instance.ErrRunExpired):
		return clierror.Wrap(err, "run_expired", "resolution")
	default:
		return clierror.Wrap(err, "evidence_unavailable", "execution")
	}
}

// Owner liveness helps diagnose interrupted runs without declaring that their
// child processes or external resources have been cleaned up.
func runOwnerAlive(record api.RunRecord) *bool {
	if record.OwnerPID <= 0 || record.State.Terminal() {
		return nil
	}
	alive := instance.ProcessAlive(record.OwnerPID)
	return &alive
}
