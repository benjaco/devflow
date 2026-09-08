package engine

import (
	"context"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

type attemptOutput struct {
	task             string
	callbackReturned bool
	callbacks        []<-chan struct{}
	resources        []attemptResource
}

type attemptResource struct {
	handle project.ServiceHandle
	done   <-chan struct{}
}

func (s *runState) trackAttemptReadiness(attemptID string, ready project.ReadyFunc) project.ReadyFunc {
	if ready == nil {
		return nil
	}
	done := make(chan struct{})
	s.mu.Lock()
	if output := s.attemptOutputs[attemptID]; output != nil {
		output.callbacks = append(output.callbacks, done)
	}
	s.mu.Unlock()
	return func(ctx context.Context, rt *project.Runtime) error {
		// A readiness timeout stops waiting, not the adapter callback. Its runtime
		// can still append diagnostics until the callback actually returns.
		defer close(done)
		return ready(ctx, rt)
	}
}

// Observe Wait once per registration. A dead resource can still have output
// writers draining, and neither readiness nor Stop alone closes its log.
func (s *runState) observeAttemptResourceLocked(attemptID string, handle project.ServiceHandle) {
	output := s.attemptOutputs[attemptID]
	if output == nil {
		return
	}
	done := make(chan struct{})
	output.resources = append(output.resources, attemptResource{handle: handle, done: done})
	go func() {
		_ = handle.Wait()
		close(done)
	}()
}

func (s *runState) finishAttemptCallback(attemptID string) {
	s.mu.Lock()
	if output := s.attemptOutputs[attemptID]; output != nil {
		output.callbackReturned = true
	}
	s.mu.Unlock()
	s.completeAttempt(attemptID)
}

func (s *runState) drainAttemptOutput(attemptID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.drainAttemptOutputContext(ctx, attemptID)
}

func (s *runState) drainAttemptOutputContext(ctx context.Context, attemptID string) {
	s.mu.Lock()
	var resources []attemptResource
	if output := s.attemptOutputs[attemptID]; output != nil {
		resources = append(resources, output.resources...)
	}
	s.mu.Unlock()
	for _, resource := range resources {
		if resource.handle.Alive() {
			continue
		}
		select {
		case <-resource.done:
		case <-ctx.Done():
			// Misbehaving external handles must not hang canceled CI forever.
			// The retained false LogsComplete keeps partial output honest.
			return
		}
	}
}

func (s *runState) completeAttempt(attemptID string) {
	var finished *api.TaskAttempt
	s.mu.Lock()
	output := s.attemptOutputs[attemptID]
	if output == nil || !output.callbackReturned || s.req.session == nil {
		s.mu.Unlock()
		return
	}
	if _, owned := s.services[output.task]; owned && s.status[output.task].AttemptID == attemptID {
		// Cleanup can still change final task state even after an early exit.
		s.mu.Unlock()
		return
	}
	for _, done := range output.callbacks {
		select {
		case <-done:
		default:
			s.mu.Unlock()
			return
		}
	}
	for _, resource := range output.resources {
		if resource.handle.Alive() {
			s.mu.Unlock()
			return
		}
		select {
		case <-resource.done:
		default:
			s.mu.Unlock()
			return
		}
	}
	session := s.req.session
	session.mu.Lock()
	for i := range session.record.Attempts {
		attempt := &session.record.Attempts[i]
		if attempt.AttemptID != attemptID || !terminalNodeState(attempt.State) || attempt.LogsComplete {
			continue
		}
		attempt.LogsComplete = true
		copy := *attempt
		copy.FailureExcerpts = append([]api.FailureExcerpt(nil), attempt.FailureExcerpts...)
		for i := range copy.FailureExcerpts {
			copy.FailureExcerpts[i].Lines = append([]string(nil), copy.FailureExcerpts[i].Lines...)
		}
		finished = &copy
		delete(s.attemptOutputs, attemptID)
		session.saveLocked()
		break
	}
	session.mu.Unlock()
	s.mu.Unlock()
	if finished != nil {
		// Lossless consumers can apply backpressure; never publish while holding
		// execution-state locks, and leave replay entirely to presentation.
		s.publishEvent(api.Event{
			TS: process.NowRFC3339Nano(), Type: api.EventTaskAttemptFinished,
			InstanceID: s.inst.ID, Worktree: s.req.Worktree, Target: s.req.Target,
			Task: finished.Task, AttemptID: finished.AttemptID, Attempt: finished,
			Mode: s.req.Mode, State: finished.State, Error: finished.LastError,
			DurationMs: durationMilliseconds(finished.FinishedAt.Sub(finished.StartedAt)),
		})
	}
}
