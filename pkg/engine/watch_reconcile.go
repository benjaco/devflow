package engine

import (
	"context"
	"fmt"
	"os"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
	"github.com/benjaco/devflow/pkg/watch"
)

// Reconcile through a fresh observation after execution and health probes.
// A queued sentinel alone cannot account for inputs edited while a task ran.
func (e *Engine) reconcileWatch(ctx context.Context, req Request, baseRT *project.Runtime, state *runState, runner *watch.Runner, batch watch.Batch, observeServices func()) error {
	pendingRequests := map[string]bool{}
	files := batch.Files
	for {
		fresh, err := runner.Sync(ctx)
		if err != nil {
			return err
		}
		files = append(files, fresh.Files...)
		userFiles, requestIDs := splitFlushSyncFiles(req.Worktree, state.inst.ID, files)
		for _, id := range requestIDs {
			pendingRequests[id] = true
		}
		files = filterProducedWatchOutputs(req.Worktree, userFiles, state.takeWatchOutputs())
		order, changed := e.affectedWatchOrder(req.Target, files)
		e.recordWatchPolicyBlocks(req.Target, state, changed, order)
		if len(order) > 0 {
			e.publish(api.Event{
				TS: process.NowRFC3339Nano(), Type: api.EventWatchCycleStart,
				InstanceID: state.inst.ID, Worktree: req.Worktree, Target: req.Target, Mode: req.Mode,
				Files: files, AffectedTasks: changed,
			})
			if err := state.stopServices(req, order); err != nil {
				return err
			}
			runErr := e.runReadyQueue(ctx, func() {}, baseRT, state, order)
			observeServices()
			e.publish(api.Event{
				TS: process.NowRFC3339Nano(), Type: api.EventWatchCycleDone,
				InstanceID: state.inst.ID, Worktree: req.Worktree, Target: req.Target, Mode: req.Mode,
				Files: files, AffectedTasks: changed, Success: boolPtr(runErr == nil),
			})
			files = nil
			continue
		}
		if len(pendingRequests) == 0 {
			return nil
		}
		results := make([]api.FlushResult, 0, len(pendingRequests))
		for _, id := range sortedBoolKeys(pendingRequests) {
			flushReq, err := instance.LoadFlushRequest(req.Worktree, state.inst.ID, id)
			if os.IsNotExist(err) {
				delete(pendingRequests, id)
				continue
			}
			if err != nil {
				return fmt.Errorf("read flush request: %w", err)
			}
			results = append(results, e.evaluateFlush(ctx, req, baseRT, state, flushReq))
		}
		fresh, err = runner.Sync(ctx)
		if err != nil {
			return err
		}
		if len(fresh.Files) > 0 {
			files = fresh.Files
			continue
		}
		for _, result := range results {
			if err := instance.WriteFlushAck(req.Worktree, state.inst.ID, result); err != nil {
				return fmt.Errorf("write flush acknowledgment: %w", err)
			}
			if err := instance.RemoveFlushRequest(req.Worktree, state.inst.ID, result.RequestID); err != nil {
				return err
			}
		}
		return nil
	}
}

func (s *runState) recordWatchOutputs(evidence watchOutputEvidence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchOutputs = append(s.watchOutputs, evidence)
}

func (s *runState) takeWatchOutputs() []watchOutputEvidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	outputs := s.watchOutputs
	s.watchOutputs = nil
	return outputs
}

func (e *Engine) recordWatchPolicyBlocks(target string, state *runState, changed, scheduled []string) {
	closure, _ := e.graph.TargetClosure(target)
	included := make(map[string]bool, len(closure))
	for _, name := range closure {
		included[name] = true
	}
	for _, name := range scheduled {
		delete(included, name)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, name := range e.watchDownstream(changed) {
		if included[name] {
			if state.watchBlocked == nil {
				state.watchBlocked = map[string]bool{}
			}
			state.watchBlocked[name] = true
		}
	}
}

func (s *runState) clearWatchBlocked(task string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.watchBlocked, task)
}

func (s *runState) isWatchBlocked(task string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchBlocked[task]
}
