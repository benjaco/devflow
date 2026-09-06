package daemon

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/benjaco/devflow/internal/clierror"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

type runOptionsKey struct{}
type runOperationKey struct{}

type runOptions struct {
	headless api.HeadlessPolicy
	deadline time.Time
}

type runOperation struct {
	id       string
	headless api.HeadlessPolicy
	cancel   context.CancelFunc
	adopted  bool
}

func requestRunContext(ctx context.Context, req Request) context.Context {
	options := runOptions{headless: req.Headless}
	if req.TimeoutMs > 0 {
		options.deadline = time.Now().Add(time.Duration(req.TimeoutMs) * time.Millisecond)
	}
	return context.WithValue(ctx, runOptionsKey{}, options)
}

func validateRunControls(req Request) error {
	if req.Headless != "" && req.Headless != api.HeadlessFail && req.Headless != api.HeadlessWait {
		return clierror.Wrap(errors.New("headless policy must be fail or wait"), "invalid_arguments", "parsing")
	}
	const maxTimeoutMillis = int64((1<<63 - 1) / time.Millisecond)
	if req.TimeoutMs < 0 || req.TimeoutMs > maxTimeoutMillis {
		return clierror.Wrap(errors.New("operation timeout is outside the supported duration range"), "invalid_arguments", "parsing")
	}
	return nil
}

func runAdmissionError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	options, _ := ctx.Value(runOptionsKey{}).(runOptions)
	if !options.deadline.IsZero() && !time.Now().Before(options.deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func (s *Server) checkRunAdmission(ctx context.Context) error {
	if err := runAdmissionError(ctx); err != nil {
		return err
	}
	if operation, ok := ctx.Value(runOperationKey{}).(*runOperation); ok && operation != nil {
		return instance.CheckRunCancellation(ctx, s.worktree, s.instanceID, operation.id)
	}
	return nil
}

func (s *Server) lockRunAdmission(ctx context.Context) error {
	if err := s.checkRunAdmission(ctx); err != nil {
		return err
	}
	if s.transitionMu.TryLock() {
		return nil
	}
	// A queued operation must be cancellable without leaving a goroutine that
	// will acquire the mutation lock later on behalf of an expired request.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.checkRunAdmission(ctx); err != nil {
				return err
			}
			if s.transitionMu.TryLock() {
				return nil
			}
		}
	}
}

func (s *Server) prepareRun(ctx context.Context, projectName, target string, mode api.RunMode, detached bool) (context.Context, *runOperation, error) {
	if operation, ok := ctx.Value(runOperationKey{}).(*runOperation); ok && operation != nil {
		return ctx, operation, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	options, _ := ctx.Value(runOptionsKey{}).(runOptions)
	if detached {
		// The observer's wait does not own detached execution. Explicit operation
		// deadlines and cancellation markers still reach the task context.
		ctx = context.WithoutCancel(ctx)
	}
	deadlineCancel := func() {}
	if !options.deadline.IsZero() {
		ctx, deadlineCancel = context.WithDeadline(ctx, options.deadline)
	}
	if err := ctx.Err(); err != nil {
		deadlineCancel()
		return nil, nil, err
	}
	record := &api.RunRecord{InstanceID: s.instanceID, Project: projectName, Target: target, Mode: mode, State: api.RunQueued, OwnerPID: os.Getpid()}
	record.Deadline, _ = ctx.Deadline()
	if err := instance.CreateRun(s.worktree, s.instanceID, record); err != nil {
		deadlineCancel()
		return nil, nil, err
	}
	observed, observerCancel, err := instance.ObserveRunCancellation(ctx, s.worktree, s.instanceID, record.RunID)
	if err != nil {
		deadlineCancel()
		_ = s.completeRun(record.RunID, nil, err)
		return nil, nil, err
	}
	operation := &runOperation{id: record.RunID, headless: options.headless, cancel: func() { observerCancel(); deadlineCancel() }}
	return context.WithValue(observed, runOperationKey{}, operation), operation, nil
}

func (s *Server) completeRun(runID string, result *api.RunResult, runErr error) error {
	record, err := instance.LoadRun(s.worktree, s.instanceID, runID)
	if err != nil {
		return err
	}
	if record.State.Terminal() {
		return nil
	}
	if result == nil {
		result = record.Result
	}
	if result == nil {
		result = &api.RunResult{RunID: runID, InstanceID: s.instanceID, Target: record.Target, Mode: record.Mode, Success: runErr == nil}
	}
	_, pruneErr := instance.PruneRuns(s.worktree, s.instanceID, instance.DefaultRunRetention)
	pruneErr = clierror.Wrap(pruneErr, "retention_failed", "execution")
	runErr = errors.Join(runErr, pruneErr)
	if runErr != nil {
		result.Success = false
		result.Error = clierror.Describe(runErr, "task_failed", "execution")
	}
	record.Result = result
	record.FinishedAt = time.Now().UTC()
	record.State = api.RunSucceeded
	if !result.Success {
		record.State = api.RunFailed
	}
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) ||
		(result.Error != nil && (result.Error.Code == "operation_cancelled" || result.Error.Code == "deadline_exceeded")) {
		record.State = api.RunCanceled
	}
	return errors.Join(pruneErr, instance.SaveRun(s.worktree, s.instanceID, record))
}

func (s *Server) finishUnadoptedRun(operation *runOperation, err error) error {
	if operation == nil || operation.adopted {
		return err
	}
	operation.cancel()
	return errors.Join(err, s.completeRun(operation.id, nil, err))
}
