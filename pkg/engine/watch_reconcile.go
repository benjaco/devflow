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
		order = e.recoverWatchPrerequisites(state, order)
		if len(order) > 0 {
			e.publish(api.Event{
				TS: process.NowRFC3339Nano(), Type: api.EventWatchCycleStart, RunID: req.RunID,
				InstanceID: state.inst.ID, Worktree: req.Worktree, Target: req.Target, Mode: req.Mode,
				Files: files, AffectedTasks: changed,
			})
			if err := state.stopServices(req, order); err != nil {
				return err
			}
			runErr := e.runReadyQueue(ctx, func() {}, baseRT, state, order)
			observeServices()
			e.publish(api.Event{
				TS: process.NowRFC3339Nano(), Type: api.EventWatchCycleDone, RunID: req.RunID,
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

// Recover only unfinished prerequisites of this change's affected work. A
// sibling canceled by an earlier failure may have no input in the new batch.
func (e *Engine) recoverWatchPrerequisites(state *runState, order []string) []string {
	selected := make(map[string]bool, len(order))
	for _, name := range order {
		selected[name] = true
	}
	visited := map[string]bool{}
	var include func(string)
	include = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		for _, dep := range e.graph.Tasks[name].Deps {
			if !selected[dep] {
				if e.prerequisiteSatisfied(state, dep) {
					continue
				}
				task := e.graph.Tasks[dep]
				if task.Kind == project.KindWarmup && !task.AllowInWatch ||
					project.IsServiceKind(task.Kind) && (task.Restart == project.RestartNever || state.isManuallyStopped(dep)) {
					continue
				}
				selected[dep] = true
			}
			include(dep)
		}
	}
	for _, name := range order {
		include(name)
	}
	// The graph was validated at construction; expanding prerequisites cannot
	// introduce cycles or unknown tasks.
	expanded, _ := e.graph.TopoSort(sortedBoolKeys(selected))
	return expanded
}

func (e *Engine) prerequisiteSatisfied(state *runState, name string) bool {
	state.mu.Lock()
	node, exists := state.status[name]
	blocked := state.watchBlocked[name]
	service := state.services[name]
	generation := state.serviceGeneration[name]
	state.mu.Unlock()
	if !exists || blocked {
		return false
	}
	if project.IsServiceKind(e.graph.Tasks[name].Kind) {
		return node.State == api.StateRunning && node.Ready && service != nil &&
			node.Generation == generation && service.Alive()
	}
	return node.State == api.StateDone || node.State == api.StateCached
}

func (s *runState) isManuallyStopped(task string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.manuallyStopped[task]
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
