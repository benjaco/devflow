package engine

import (
	"context"
	"errors"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
)

const promptWaitLimit = 5 * time.Minute

func (e *Engine) waitForPromptAnswer(ctx context.Context, req Request, instanceID, taskName, attemptID string, request process.PromptRequest) (response process.PromptResponse, err error) {
	waitCtx, cancel := context.WithTimeout(ctx, promptWaitLimit)
	defer cancel()
	deadline, _ := waitCtx.Deadline()
	state := api.PromptPending
	if req.Headless != api.HeadlessWait {
		// A failed operation keeps a diagnostic, never a briefly answerable request.
		state = api.PromptCancelled
	}
	prompt, err := instance.CreatePrompt(waitCtx, req.Worktree, instanceID, api.Prompt{
		RunID: req.RunID, Task: taskName, AttemptID: attemptID,
		Kind: string(request.Kind), Message: request.Prompt, Secret: request.Secret,
		State: state, Deadline: deadline,
	})
	if err != nil {
		return process.PromptResponse{}, err
	}
	waiting := req.Headless == api.HeadlessWait
	defer func() {
		state := api.PromptCancelled
		if errors.Is(err, context.DeadlineExceeded) {
			state = api.PromptExpired
		}
		// The execution context is already cancelled on this path; cleanup must
		// still remove an answer that arrived just before cancellation won.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		err = errors.Join(err, instance.ClosePrompt(cleanupCtx, req.Worktree, instanceID, req.RunID, prompt.ID, state))
		if waiting {
			err = errors.Join(err, req.session.setWaiting(-1))
		}
		if err != nil {
			e.publishPromptEvent(req, instanceID, prompt, api.EventInteractionStop, err)
		}
	}()
	if waiting {
		if err := req.session.setWaiting(1); err != nil {
			return process.PromptResponse{}, err
		}
	}
	e.publishPromptEvent(req, instanceID, prompt, api.EventInteractionReq, nil)
	if req.Headless != api.HeadlessWait {
		return process.PromptResponse{}, &api.CommandError{
			Code: "interaction_required", Phase: "execution",
			Message: "task " + taskName + " requires a " + prompt.Kind + " response; supply action inputs or explicitly rerun with --headless wait",
		}
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return process.PromptResponse{}, waitCtx.Err()
		case <-ticker.C:
			answer, err := instance.ConsumePromptAnswer(waitCtx, req.Worktree, instanceID, req.RunID, prompt.ID)
			if err != nil {
				return process.PromptResponse{}, err
			}
			if answer == nil {
				continue
			}
			if answer.Confirm != nil {
				response.Value = "n"
				if *answer.Confirm {
					response.Value = "y"
				}
			} else if answer.Text != nil {
				response.Value = *answer.Text
			}
			e.publishPromptEvent(req, instanceID, prompt, api.EventInteractionAck, nil)
			return response, nil
		}
	}
}

func (s *runSession) setWaiting(delta int) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingPrompts += delta
	s.record.State = api.RunRunning
	if s.pendingPrompts > 0 {
		s.record.State = api.RunWaiting
	}
	s.saveLocked()
	return s.err
}

func (e *Engine) publishPromptEvent(req Request, instanceID string, prompt api.Prompt, eventType api.EventType, err error) {
	evt := api.Event{
		TS: process.NowRFC3339Nano(), Type: eventType,
		InstanceID: instanceID, RunID: req.RunID, Worktree: req.Worktree, Target: req.Target,
		Task: prompt.Task, AttemptID: prompt.AttemptID, Mode: req.Mode,
		PromptID: prompt.ID, PromptKind: prompt.Kind, Prompt: prompt.Message, PromptSecret: prompt.Secret,
	}
	if err != nil {
		evt.Error = err.Error()
	}
	e.publish(evt)
}
