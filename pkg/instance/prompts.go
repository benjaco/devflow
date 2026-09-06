package instance

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/benjaco/devflow/internal/jsonutil"
	"github.com/benjaco/devflow/pkg/api"
)

func CreatePrompt(ctx context.Context, worktree, instanceID string, prompt api.Prompt) (api.Prompt, error) {
	if err := ctx.Err(); err != nil {
		return api.Prompt{}, err
	}
	if prompt.Kind != "confirm" && prompt.Kind != "text" {
		return api.Prompt{}, promptError("invalid_prompt", "prompt kind must be confirm or text")
	}
	if prompt.Task == "" || ValidateAttemptID(prompt.AttemptID) != nil {
		return api.Prompt{}, promptError("invalid_prompt", "prompt requires a task and valid attempt ID")
	}
	if len(prompt.Message) > 64<<10 {
		return api.Prompt{}, promptError("invalid_prompt", "prompt message exceeds 64 KiB")
	}
	if prompt.State == "" {
		prompt.State = api.PromptPending
	}
	if prompt.State != api.PromptPending && prompt.State != api.PromptCancelled && prompt.State != api.PromptExpired {
		return api.Prompt{}, promptError("invalid_prompt", "new prompt must be pending or closed")
	}
	prompt.ID = "prompt-" + rand.Text()
	prompt.CreatedAt = time.Now().UTC()
	if prompt.State == api.PromptPending && !prompt.Deadline.IsZero() && !prompt.CreatedAt.Before(prompt.Deadline) {
		prompt.State = api.PromptExpired
	}
	path, err := promptPath(worktree, instanceID, prompt.RunID, prompt.ID)
	if err != nil {
		return api.Prompt{}, err
	}
	err = withRunStoreLock(ctx, worktree, instanceID, false, func() error {
		run, err := loadRunLocked(worktree, instanceID, prompt.RunID)
		if err != nil {
			return err
		}
		if run.State.Terminal() {
			return promptError("prompt_not_pending", "prompt operation has finished")
		}
		if !promptAttemptActive(run, prompt) {
			return promptError("prompt_not_pending", "prompt attempt is no longer current or active")
		}
		if err := CheckRunCancellation(ctx, worktree, instanceID, prompt.RunID); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return jsonutil.WriteFileAtomic(path, prompt)
	})
	if err != nil {
		return api.Prompt{}, promptRunError(err)
	}
	return prompt, nil
}

// RespondPrompt serializes admission with cancellation across separate CLI processes.
func RespondPrompt(ctx context.Context, worktree, instanceID string, answer api.PromptAnswer) error {
	return withPrompt(ctx, worktree, instanceID, answer.RunID, answer.PromptID, func(path string, prompt *api.Prompt) error {
		if prompt.RunID != answer.RunID || prompt.Task != answer.Task || prompt.AttemptID != answer.AttemptID {
			return promptError("prompt_mismatch", "response does not match the prompt's run, task and attempt")
		}
		if prompt.State != api.PromptPending {
			return promptError("prompt_not_pending", "prompt is no longer answerable")
		}
		if !prompt.Deadline.IsZero() && !time.Now().Before(prompt.Deadline) {
			prompt.State = api.PromptExpired
			if err := jsonutil.WriteFileAtomic(path, prompt); err != nil {
				return err
			}
			return promptError("prompt_not_pending", "prompt deadline has expired")
		}
		run, err := loadRunLocked(worktree, instanceID, answer.RunID)
		if err != nil {
			return err
		}
		if run.State.Terminal() {
			return promptError("prompt_not_pending", "prompt operation has finished")
		}
		if !promptAttemptActive(run, *prompt) {
			return promptError("prompt_not_pending", "prompt attempt is no longer current or active")
		}
		if err := CheckRunCancellation(ctx, worktree, instanceID, answer.RunID); err != nil {
			if !errors.Is(err, context.Canceled) {
				return err
			}
			prompt.State = api.PromptCancelled
			if err := jsonutil.WriteFileAtomic(path, prompt); err != nil {
				return err
			}
			return promptError("prompt_not_pending", "prompt operation was cancelled")
		}
		if (prompt.Kind == "confirm" && (answer.Confirm == nil || answer.Text != nil)) ||
			(prompt.Kind == "text" && (answer.Text == nil || answer.Confirm != nil)) {
			return promptError("invalid_prompt_answer", "confirm prompts require a boolean; text prompts require a string")
		}
		if answer.Text != nil && len(*answer.Text) > 1<<20 {
			return promptError("invalid_prompt_answer", "text response exceeds 1 MiB")
		}
		answerPath := promptAnswerPath(path)
		if err := os.MkdirAll(filepath.Dir(answerPath), 0o700); err != nil {
			return err
		}
		if err := jsonutil.WriteFileAtomic(answerPath, answer); err != nil {
			return err
		}
		prompt.State = api.PromptAnswered
		if err := jsonutil.WriteFileAtomic(path, prompt); err != nil {
			return errors.Join(err, removePromptAnswer(answerPath))
		}
		return nil
	})
}

func ConsumePromptAnswer(ctx context.Context, worktree, instanceID, runID, promptID string) (*api.PromptAnswer, error) {
	var answer *api.PromptAnswer
	err := withPrompt(ctx, worktree, instanceID, runID, promptID, func(path string, prompt *api.Prompt) error {
		if err := CheckRunCancellation(ctx, worktree, instanceID, runID); err != nil {
			return errors.Join(err, removePromptAnswer(promptAnswerPath(path)))
		}
		run, err := loadRunLocked(worktree, instanceID, runID)
		if err != nil {
			return err
		}
		if !promptAttemptActive(run, *prompt) {
			return errors.Join(promptError("prompt_not_pending", "prompt attempt is no longer current or active"), removePromptAnswer(promptAnswerPath(path)))
		}
		if prompt.State != api.PromptAnswered {
			return nil
		}
		var value api.PromptAnswer
		if err := jsonutil.ReadFile(promptAnswerPath(path), &value); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := removePromptAnswer(promptAnswerPath(path)); err != nil {
			return err
		}
		answer = &value
		return nil
	})
	return answer, err
}

func ClosePrompt(ctx context.Context, worktree, instanceID, runID, promptID string, state api.PromptState) error {
	if state != api.PromptCancelled && state != api.PromptExpired {
		return promptError("invalid_prompt", "prompt closure must be cancelled or expired")
	}
	return withPrompt(ctx, worktree, instanceID, runID, promptID, func(path string, prompt *api.Prompt) error {
		if err := removePromptAnswer(promptAnswerPath(path)); err != nil {
			return err
		}
		if prompt.State != api.PromptPending {
			return nil
		}
		prompt.State = state
		return jsonutil.WriteFileAtomic(path, prompt)
	})
}

func CloseRunPrompts(ctx context.Context, worktree, instanceID, runID string, state api.PromptState) error {
	prompts, err := ListPrompts(ctx, worktree, instanceID, runID)
	if err != nil {
		return err
	}
	var errs []error
	for _, prompt := range prompts {
		if err := ClosePrompt(ctx, worktree, instanceID, runID, prompt.ID, state); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func ListPrompts(ctx context.Context, worktree, instanceID, runID string) ([]api.Prompt, error) {
	runPath, err := RunPath(worktree, instanceID, runID)
	if err != nil {
		return nil, err
	}
	prompts := []api.Prompt{}
	err = withRunStoreLock(ctx, worktree, instanceID, false, func() error {
		run, err := loadRunLocked(worktree, instanceID, runID)
		if err != nil {
			return err
		}
		cancelErr := CheckRunCancellation(ctx, worktree, instanceID, runID)
		if cancelErr != nil && !errors.Is(cancelErr, context.Canceled) {
			return cancelErr
		}
		entries, err := os.ReadDir(filepath.Join(runPath, "prompts"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			path, err := promptPath(worktree, instanceID, runID, id)
			if err != nil {
				return err
			}
			prompt, err := loadPromptLocked(path, runID, id)
			if err != nil {
				return err
			}
			if prompt.State == api.PromptPending {
				switch {
				case !prompt.Deadline.IsZero() && !time.Now().Before(prompt.Deadline):
					prompt.State = api.PromptExpired
				case !promptAttemptActive(run, *prompt) || errors.Is(cancelErr, context.Canceled):
					prompt.State = api.PromptCancelled
				}
				// Inspection derives expiry without mutating retained evidence.
				// Response admission rechecks the same conditions under this lock.
			}
			prompts = append(prompts, *prompt)
		}
		return nil
	})
	if err != nil {
		return nil, promptRunError(err)
	}
	sort.Slice(prompts, func(i, j int) bool { return prompts[i].CreatedAt.Before(prompts[j].CreatedAt) })
	return prompts, nil
}

func promptAttemptActive(run *api.RunRecord, prompt api.Prompt) bool {
	if run.State.Terminal() {
		return false
	}
	// Only the newest attempt for this task may request or consume input.
	// An old service reader can outlive Stop while its prompt is unwinding.
	for i := len(run.Attempts) - 1; i >= 0; i-- {
		attempt := run.Attempts[i]
		if attempt.Task != prompt.Task {
			continue
		}
		if attempt.AttemptID != prompt.AttemptID {
			return false
		}
		switch attempt.State {
		case api.StateStarting, api.StateRunning, api.StateReady, api.StateRestarting:
			return true
		default:
			return false
		}
	}
	return false
}

func withPrompt(ctx context.Context, worktree, instanceID, runID, promptID string, fn func(string, *api.Prompt) error) error {
	path, err := promptPath(worktree, instanceID, runID, promptID)
	if err != nil {
		return err
	}
	// The run-store lock also excludes finalization and pruning, so response
	// admission cannot recreate expired evidence or race a terminal result.
	err = withRunStoreLock(ctx, worktree, instanceID, false, func() error {
		if _, err := loadRunLocked(worktree, instanceID, runID); err != nil {
			return err
		}
		prompt, err := loadPromptLocked(path, runID, promptID)
		if err != nil {
			return err
		}
		return fn(path, prompt)
	})
	return promptRunError(err)
}

func promptRunError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return ErrRunUnknown
	}
	return err
}

func loadPromptLocked(path, runID, promptID string) (*api.Prompt, error) {
	var prompt api.Prompt
	if err := jsonutil.ReadFile(path, &prompt); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, promptError("unknown_prompt", "prompt was not found in this run")
		}
		return nil, err
	}
	if prompt.ID != promptID || prompt.RunID != runID {
		return nil, promptError("prompt_mismatch", "stored prompt identity does not match its path")
	}
	if prompt.Kind != "confirm" && prompt.Kind != "text" {
		return nil, promptError("invalid_prompt", "stored prompt kind must be confirm or text")
	}
	return &prompt, nil
}

func promptPath(worktree, instanceID, runID, promptID string) (string, error) {
	if !strings.HasPrefix(promptID, "prompt-") || len(promptID) != len("prompt-")+26 {
		return "", promptError("invalid_prompt", "invalid prompt ID")
	}
	for _, ch := range strings.TrimPrefix(promptID, "prompt-") {
		if !(ch >= 'A' && ch <= 'Z') && !(ch >= '2' && ch <= '7') {
			return "", promptError("invalid_prompt", "invalid prompt ID")
		}
	}
	runPath, err := RunPath(worktree, instanceID, runID)
	if err != nil {
		return "", err
	}
	return filepath.Join(runPath, "prompts", promptID+".json"), nil
}

func promptAnswerPath(path string) string {
	return filepath.Join(filepath.Dir(path), "answers", filepath.Base(path))
}

func removePromptAnswer(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func promptError(code, message string) error {
	return &api.CommandError{Code: code, Phase: "interaction", Message: message}
}
