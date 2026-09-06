package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/benjaco/devflow/internal/clierror"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/fingerprint"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

type runSession struct {
	pendingPrompts int
	mu             sync.Mutex
	worktree       string
	record         *api.RunRecord
	err            error
}

var executableVersion = sync.OnceValues(func() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	// The compiled adapter contains callback code that task metadata cannot describe.
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
})

func (e *Engine) beginRun(ctx context.Context, req *Request) (context.Context, func(), error) {
	id, _, err := instance.IDForWorktree(req.Worktree)
	if err != nil {
		return ctx, func() {}, err
	}
	if req.Headless != "" && req.Headless != api.HeadlessFail && req.Headless != api.HeadlessWait {
		return ctx, func() {}, clierror.Wrap(errors.New("headless policy must be fail or wait"), "invalid_arguments", "parsing")
	}
	if req.Timeout < 0 {
		return ctx, func() {}, clierror.Wrap(errors.New("execution timeout must not be negative"), "invalid_arguments", "parsing")
	}
	deadlineCancel := func() {}
	if req.Timeout > 0 {
		ctx, deadlineCancel = context.WithTimeout(ctx, req.Timeout)
	}
	signatures := map[string]string{}
	for name, task := range e.graph.Tasks {
		signature, err := fingerprint.TaskSignature(task)
		if err != nil {
			deadlineCancel()
			return ctx, func() {}, err
		}
		signatures[name] = signature
	}
	data, err := json.Marshal(struct {
		Tasks   map[string]string
		Targets map[string]project.Target
	}{signatures, e.graph.Targets})
	if err != nil {
		deadlineCancel()
		return ctx, func() {}, err
	}
	digest := sha256.Sum256(data)
	adapter, err := executableVersion()
	if err != nil {
		deadlineCancel()
		return ctx, func() {}, err
	}
	if req.RunID == "" {
		record := &api.RunRecord{InstanceID: id, Project: e.project.Name(), Target: req.Target, Mode: req.Mode, OwnerPID: os.Getpid()}
		record.Deadline, _ = ctx.Deadline()
		if err := instance.CreateRun(req.Worktree, id, record); err != nil {
			deadlineCancel()
			return ctx, func() {}, err
		}
		req.RunID = record.RunID
	}
	selected, err := instance.LoadRun(req.Worktree, id, req.RunID)
	if err != nil {
		deadlineCancel()
		return ctx, func() {}, err
	}
	if selected.Project != e.project.Name() || selected.Target != req.Target || selected.Mode != req.Mode {
		deadlineCancel()
		return ctx, func() {}, clierror.Wrap(errors.New("run identity does not match project, target and mode"), "run_mismatch", "admission")
	}
	record, err := instance.ClaimRun(req.Worktree, id, req.RunID, os.Getpid())
	if err != nil {
		deadlineCancel()
		return ctx, func() {}, err
	}
	record.GraphDigest = hex.EncodeToString(digest[:])
	record.AdapterVersion = adapter
	req.session = &runSession{worktree: req.Worktree, record: record}
	req.session.saveLocked()
	observed, cancel, err := instance.ObserveRunCancellation(ctx, req.Worktree, id, req.RunID)
	if err != nil {
		deadlineCancel()
		return ctx, func() {}, err
	}
	return observed, func() { cancel(); deadlineCancel() }, req.session.err
}

func (s *runSession) saveLocked() {
	if err := instance.SaveRun(s.worktree, s.record.InstanceID, s.record); err != nil && s.err == nil {
		s.err = err
	}
}

func (s *runSession) finish(result *api.RunResult, runErr error, deferCompletion bool) error {
	if s == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	promptErr := instance.CloseRunPrompts(cleanupCtx, s.worktree, s.record.InstanceID, s.record.RunID, api.PromptCancelled)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = errors.Join(s.err, promptErr)
	if !deferCompletion {
		// Retention errors belong in the terminal result, before it becomes immutable.
		_, pruneErr := instance.PruneRuns(s.worktree, s.record.InstanceID, instance.DefaultRunRetention)
		s.err = errors.Join(s.err, clierror.Wrap(pruneErr, "retention_failed", "execution"))
	}
	if s.err != nil {
		result.Success = false
		result.Error = clierror.Describe(errors.Join(runErr, s.err), "evidence_write_failed", "execution")
	}
	if runErr != nil && result.Success {
		result.Success = false
		result.Error = clierror.Describe(runErr, "task_failed", "execution")
	}
	s.record.Result = result
	if !deferCompletion {
		s.record.State = api.RunSucceeded
		if !result.Success {
			s.record.State = api.RunFailed
		}
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			s.record.State = api.RunCanceled
		}
		s.record.FinishedAt = time.Now().UTC()
	}
	s.saveLocked()
	return s.err
}

func (e *Engine) beginAttempt(ctx context.Context, state *runState, rt *project.Runtime, task project.Task) error {
	s := state.req.session
	if s == nil {
		return errors.New("task execution requires a retained run")
	}
	rt.RunID = state.req.RunID
	rt.AttemptID = instance.NewAttemptID()
	path, err := instance.AttemptLogPath(rt.Worktree, state.inst.ID, rt.RunID, rt.AttemptID)
	if err != nil {
		return err
	}
	rt.LogPath = path
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	rt.OnPrompt = func(_ string, prompt process.PromptRequest) (process.PromptResponse, error) {
		return e.waitForPromptAnswer(ctx, state.req, state.inst.ID, task.Name, rt.AttemptID, prompt)
	}
	state.mu.Lock()
	node := state.status[task.Name]
	node.RunID = rt.RunID
	node.AttemptID = rt.AttemptID
	node.LogPath = rt.LogPath
	node.Attempt++
	node.LastError = ""
	node.LastRunKey = ""
	node.FailureExcerpts = nil
	state.status[task.Name] = node
	state.nodeStarted[task.Name] = time.Now()
	s.mu.Lock()
	s.record.Attempts = append(s.record.Attempts, api.TaskAttempt{AttemptID: rt.AttemptID, Task: task.Name, State: api.StateRunning, LogPath: path, StartedAt: time.Now().UTC()})
	s.saveLocked()
	err = s.err
	s.mu.Unlock()
	state.mu.Unlock()
	if err != nil {
		return err
	}
	next := api.StateRunning
	if project.IsServiceKind(task.Kind) {
		next = api.StateStarting
	}
	state.setNodeState(task.Name, next, "", "", 0)
	return nil
}

func (s *runState) markExecuted(attemptID string) {
	session := s.req.session
	if session == nil {
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	for i := range session.record.Attempts {
		if session.record.Attempts[i].AttemptID == attemptID {
			session.record.Attempts[i].Executed = true
			break
		}
	}
	session.saveLocked()
}

// Caller holds state.mu; attempts retain their own state when a later retry replaces the node.
func (s *runState) saveAttemptsLocked() {
	session := s.req.session
	if session == nil {
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	for i := range session.record.Attempts {
		attempt := &session.record.Attempts[i]
		node := s.status[attempt.Task]
		if node.AttemptID != attempt.AttemptID {
			continue
		}
		attempt.State = node.State
		attempt.CacheKey = node.LastRunKey
		attempt.LastError = node.LastError
		attempt.FailureExcerpts = node.FailureExcerpts
		if terminalNodeState(node.State) && attempt.FinishedAt.IsZero() {
			attempt.FinishedAt = time.Now().UTC()
		}
	}
	session.saveLocked()
}
