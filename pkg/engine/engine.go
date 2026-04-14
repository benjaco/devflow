package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"

	"devflow/pkg/api"
	"devflow/pkg/cache"
	"devflow/pkg/event"
	"devflow/pkg/fingerprint"
	"devflow/pkg/graph"
	"devflow/pkg/instance"
	"devflow/pkg/ports"
	"devflow/pkg/process"
	"devflow/pkg/project"
	"devflow/pkg/watch"
)

type Request struct {
	Target      string
	Worktree    string
	Mode        api.RunMode
	MaxParallel int
}

type Outcome struct {
	Result   api.RunResult
	Instance *api.Instance
}

type Engine struct {
	project project.Project
	graph   *graph.Graph
	cache   *cache.Store
	ports   *ports.Manager
	events  event.Bus[api.Event]
}

type runState struct {
	mu        sync.Mutex
	req       Request
	inst      *api.Instance
	status    map[string]api.NodeStatus
	depKeys   map[string]string
	cacheHits []string
	services  map[string]*process.Handle
	publish   func(api.Event)
}

type taskResult struct {
	name   string
	key    string
	cached bool
	err    error
}

func New(p project.Project, worktree string) (*Engine, error) {
	g, err := graph.New(p.Tasks(), p.Targets())
	if err != nil {
		return nil, err
	}
	pm, err := ports.NewDefault()
	if err != nil {
		return nil, err
	}
	return &Engine{
		project: p,
		graph:   g,
		cache:   cache.New(instance.CacheRoot(worktree)),
		ports:   pm,
	}, nil
}

func (e *Engine) Graph() *graph.Graph {
	return e.graph
}

func (e *Engine) SubscribeEvents() <-chan api.Event {
	return e.events.Subscribe()
}

func (e *Engine) Watch(ctx context.Context, req Request) error {
	started := time.Now().UTC()
	inst, state, baseRT, err := e.prepareExecution(ctx, req)
	if err != nil {
		return err
	}

	e.publish(api.Event{
		TS:         process.NowRFC3339Nano(),
		Type:       api.EventRunStarted,
		InstanceID: inst.ID,
		Worktree:   req.Worktree,
		Target:     req.Target,
		Mode:       req.Mode,
	})

	order, err := e.graph.TargetClosure(req.Target)
	if err != nil {
		return err
	}

	initialSuccess := true
	if err := e.runReadyQueue(ctx, func() {}, baseRT, state, order); err != nil {
		initialSuccess = false
	}
	e.publish(api.Event{
		TS:         process.NowRFC3339Nano(),
		Type:       api.EventWatchCycleDone,
		InstanceID: inst.ID,
		Worktree:   req.Worktree,
		Target:     req.Target,
		Mode:       req.Mode,
		Success:    boolPtr(initialSuccess),
	})

	runner, err := watch.New(watch.Options{Root: req.Worktree})
	if err != nil {
		return err
	}
	batches, errs, err := runner.Start(ctx)
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			e.stopAllServices(req, inst, state)
			e.publishRunFinished(api.RunResult{
				Target:     req.Target,
				Mode:       req.Mode,
				InstanceID: inst.ID,
				Success:    true,
				DurationMs: time.Since(started).Milliseconds(),
				StartedAt:  started.Format(time.RFC3339),
				FinishedAt: time.Now().UTC().Format(time.RFC3339),
			}, req.Worktree, "")
			return nil
		case err := <-errs:
			if err == nil {
				continue
			}
			return err
		case batch, ok := <-batches:
			if !ok {
				e.stopAllServices(req, inst, state)
				return nil
			}
			if len(batch.Files) == 0 {
				continue
			}
			affectedOrder, changedTasks := e.affectedWatchOrder(req.Target, batch.Files)
			if len(affectedOrder) == 0 {
				continue
			}
			e.publish(api.Event{
				TS:            process.NowRFC3339Nano(),
				Type:          api.EventWatchCycleStart,
				InstanceID:    inst.ID,
				Worktree:      req.Worktree,
				Target:        req.Target,
				Mode:          req.Mode,
				Files:         append([]string(nil), batch.Files...),
				AffectedTasks: changedTasks,
			})
			state.stopServices(req, affectedOrder)
			success := true
			if err := e.runReadyQueue(ctx, func() {}, baseRT, state, affectedOrder); err != nil {
				success = false
			}
			e.publish(api.Event{
				TS:            process.NowRFC3339Nano(),
				Type:          api.EventWatchCycleDone,
				InstanceID:    inst.ID,
				Worktree:      req.Worktree,
				Target:        req.Target,
				Mode:          req.Mode,
				Files:         append([]string(nil), batch.Files...),
				AffectedTasks: changedTasks,
				Success:       boolPtr(success),
			})
		}
	}
}

func (e *Engine) Run(ctx context.Context, req Request) (*Outcome, error) {
	started := time.Now().UTC()
	inst, state, baseRT, err := e.prepareExecution(ctx, req)
	if err != nil {
		return nil, err
	}

	order, err := e.graph.TargetClosure(req.Target)
	if err != nil {
		return nil, err
	}

	result := api.RunResult{
		Target:     req.Target,
		Mode:       req.Mode,
		InstanceID: inst.ID,
		Success:    false,
		StartedAt:  started.Format(time.RFC3339),
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.publish(api.Event{
		TS:         process.NowRFC3339Nano(),
		Type:       api.EventRunStarted,
		InstanceID: inst.ID,
		Worktree:   req.Worktree,
		Target:     req.Target,
		Mode:       req.Mode,
	})

	if err := e.runReadyQueue(runCtx, cancel, baseRT, state, order); err != nil {
		result.FailedNode = state.failedNode()
		result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		result.DurationMs = time.Since(started).Milliseconds()
		e.publishRunFinished(result, req.Worktree, err.Error())
		return &Outcome{Result: result, Instance: inst}, err
	}

	result.Success = true
	result.CacheHits = state.snapshotCacheHits()

	services := state.snapshotServices()
	if len(services) > 0 && req.Mode != api.ModeCI {
		waitErr := e.waitForServices(ctx, req, inst, state.statusSnapshot(), services)
		if waitErr != nil && !errors.Is(waitErr, context.Canceled) {
			result.Success = false
			result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			result.DurationMs = time.Since(started).Milliseconds()
			e.publishRunFinished(result, req.Worktree, waitErr.Error())
			return nil, waitErr
		}
	}

	result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	result.DurationMs = time.Since(started).Milliseconds()
	if err := instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, state.statusSnapshot()); err != nil {
		return nil, err
	}
	e.publishRunFinished(result, req.Worktree, "")
	return &Outcome{Result: result, Instance: inst}, nil
}

func (e *Engine) prepareExecution(ctx context.Context, req Request) (*api.Instance, *runState, *project.Runtime, error) {
	inst, err := instance.Resolve(req.Worktree, filepath.Base(req.Worktree))
	if err != nil {
		return nil, nil, nil, err
	}

	cfg, err := e.project.ConfigureInstance(ctx, req.Worktree)
	if err != nil {
		return nil, nil, nil, err
	}
	inst.Label = cfg.Label
	inst.DB = cfg.DB
	inst.Env = mergeInstanceEnv(inst.Env, cfg.Env, map[string]string{
		"DEVFLOW_INSTANCE_ID": inst.ID,
		"DEVFLOW_WORKTREE":    inst.Worktree,
	})
	if len(cfg.PortNames) > 0 {
		inst.Ports, err = e.ports.Allocate(inst.ID, cfg.PortNames)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if cfg.Finalize != nil {
		if err := cfg.Finalize(inst); err != nil {
			return nil, nil, nil, err
		}
	}
	if err := instance.Save(inst); err != nil {
		return nil, nil, nil, err
	}

	order, err := e.graph.TargetClosure(req.Target)
	if err != nil {
		return nil, nil, nil, err
	}

	state := &runState{
		req:      req,
		inst:     inst,
		status:   map[string]api.NodeStatus{},
		depKeys:  map[string]string{},
		services: map[string]*process.Handle{},
		publish:  e.publish,
	}
	for _, name := range order {
		task := e.graph.Tasks[name]
		state.status[name] = api.NodeStatus{
			Name:    name,
			Kind:    string(task.Kind),
			State:   api.StatePending,
			LogPath: instance.LogPath(req.Worktree, inst.ID, name),
		}
	}
	if err := instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, state.status); err != nil {
		return nil, nil, nil, err
	}
	e.publish(api.Event{
		TS:         process.NowRFC3339Nano(),
		Type:       api.EventInstanceUpdated,
		InstanceID: inst.ID,
		Worktree:   req.Worktree,
		Target:     req.Target,
		Mode:       req.Mode,
	})

	baseRT := &project.Runtime{
		Worktree: req.Worktree,
		Instance: inst,
		Mode:     req.Mode,
		Env:      cloneMap(inst.Env),
		EventFn: func(evt api.Event) {
			e.publish(evt)
		},
		OnService: func(task string, handle *process.Handle) {
			state.registerService(task, handle)
		},
		OnPrompt: func(task string, prompt process.PromptRequest) (process.PromptResponse, error) {
			return e.waitForPromptAnswer(ctx, req, inst.ID, task, prompt)
		},
	}
	return inst, state, baseRT, nil
}

func (e *Engine) runReadyQueue(ctx context.Context, cancel context.CancelFunc, baseRT *project.Runtime, state *runState, order []string) error {
	subset := make(map[string]bool, len(order))
	for _, name := range order {
		subset[name] = true
	}

	pendingDeps := map[string]int{}
	dependents := map[string][]string{}
	for _, name := range order {
		task := e.graph.Tasks[name]
		count := 0
		for _, dep := range task.Deps {
			if subset[dep] {
				count++
				dependents[dep] = append(dependents[dep], name)
			}
		}
		pendingDeps[name] = count
	}

	ready := make([]string, 0, len(order))
	for _, name := range order {
		if pendingDeps[name] == 0 {
			ready = append(ready, name)
		}
	}

	maxParallel := normalizeMaxParallel(state.req.MaxParallel)
	results := make(chan taskResult, len(order))
	var wg sync.WaitGroup
	running := 0
	completed := 0
	failed := false
	var runErr error

	for completed < len(order) {
		for !failed && running < maxParallel && len(ready) > 0 {
			sortReadyQueue(ready, e.graph)
			name := ready[0]
			ready = ready[1:]
			task := e.graph.Tasks[name]

			if task.Kind == project.KindGroup {
				state.setNodeState(name, api.StateDone, "", "", 0)
				for _, child := range dependents[name] {
					pendingDeps[child]--
					if pendingDeps[child] == 0 {
						ready = append(ready, child)
					}
				}
				completed++
				continue
			}

			state.setNodeState(name, api.StateReady, "", "", 0)
			if task.Kind != project.KindService {
				state.setNodeState(name, api.StateRunning, "", "", 0)
			}

			depKeys := state.depKeySnapshot(task.Deps)
			rt := baseRT.WithTask(name, instance.LogPath(state.req.Worktree, state.inst.ID, name))
			rt.DepKeys = append([]string(nil), depKeys...)
			wg.Add(1)
			running++
			go func(task project.Task, taskName string, depSnapshot []string, runtime *project.Runtime) {
				defer wg.Done()
				results <- e.executeTask(ctx, state, runtime, task, depSnapshot)
			}(task, name, depKeys, rt)
		}

		if completed >= len(order) {
			break
		}

		if running == 0 {
			if failed {
				break
			}
			return fmt.Errorf("scheduler stalled before completing target %q", state.req.Target)
		}

		res := <-results
		running--
		completed++

		if res.err != nil {
			failed = true
			runErr = res.err
			cancel()
			continue
		}

		if res.key != "" {
			state.setDepKey(res.name, res.key)
		}
		if res.cached {
			state.addCacheHit(res.name)
		}
		for _, child := range dependents[res.name] {
			pendingDeps[child]--
			if pendingDeps[child] == 0 {
				ready = append(ready, child)
			}
		}
	}

	wg.Wait()
	return runErr
}

func (e *Engine) executeTask(ctx context.Context, state *runState, rt *project.Runtime, task project.Task, depKeys []string) taskResult {
	if task.Cache {
		key, err := e.taskKey(ctx, rt, task, depKeys)
		if err != nil {
			state.setErrorState(task.Name, ctx, "", err, 0)
			return taskResult{name: task.Name, err: err}
		}
		state.setLastRunKey(task.Name, key)
		if ok, restoreErr := e.cache.Restore(rt.Worktree, task.Name, key); restoreErr == nil && ok {
			state.publishEvent(api.Event{
				TS:         process.NowRFC3339Nano(),
				Type:       api.EventCacheHit,
				InstanceID: state.inst.ID,
				Worktree:   state.req.Worktree,
				Target:     state.req.Target,
				Task:       task.Name,
				Mode:       state.req.Mode,
				CacheKey:   key,
			})
			state.setNodeState(task.Name, api.StateCached, key, "", 0)
			return taskResult{name: task.Name, key: key, cached: true}
		}
		state.publishEvent(api.Event{
			TS:         process.NowRFC3339Nano(),
			Type:       api.EventCacheMiss,
			InstanceID: state.inst.ID,
			Worktree:   state.req.Worktree,
			Target:     state.req.Target,
			Task:       task.Name,
			Mode:       state.req.Mode,
			CacheKey:   key,
		})
		if err := runTask(ctx, task, rt); err != nil {
			state.setErrorState(task.Name, ctx, key, err, 0)
			return taskResult{name: task.Name, key: key, err: err}
		}
		if _, err := e.cache.Snapshot(rt.Worktree, task, key); err != nil {
			state.setErrorState(task.Name, ctx, key, err, 0)
			return taskResult{name: task.Name, key: key, err: err}
		}
		state.setNodeState(task.Name, api.StateDone, key, "", 0)
		return taskResult{name: task.Name, key: key}
	}

	if err := runTask(ctx, task, rt); err != nil {
		state.setErrorState(task.Name, ctx, "", err, 0)
		return taskResult{name: task.Name, err: err}
	}
	if task.Kind == project.KindService {
		handle, ok := state.serviceHandle(task.Name)
		if !ok {
			err := fmt.Errorf("service task %q returned without starting a service", task.Name)
			state.setErrorState(task.Name, ctx, "", err, 0)
			return taskResult{name: task.Name, err: err}
		}
		if err := e.awaitServiceReady(ctx, rt, task, handle); err != nil {
			_ = handle.Stop()
			state.removeService(task.Name)
			state.setErrorState(task.Name, ctx, "", err, 0)
			return taskResult{name: task.Name, err: err}
		}
		state.setNodeState(task.Name, api.StateRunning, "", "", handle.PID())
		return taskResult{name: task.Name}
	}
	state.setNodeState(task.Name, api.StateDone, "", "", 0)
	return taskResult{name: task.Name}
}

func (e *Engine) awaitServiceReady(ctx context.Context, rt *project.Runtime, task project.Task, handle *process.Handle) error {
	if task.Ready == nil {
		return nil
	}
	timeout := task.ReadyTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	readyCh := make(chan error, 1)
	exitCh := make(chan error, 1)
	go func() {
		readyCh <- task.Ready(readyCtx, rt)
	}()
	go func() {
		exitCh <- handle.Wait()
	}()

	select {
	case err := <-readyCh:
		if err != nil {
			return err
		}
		return nil
	case err := <-exitCh:
		if err != nil {
			return fmt.Errorf("service exited before readiness: %w", err)
		}
		return fmt.Errorf("service exited before readiness")
	case <-readyCtx.Done():
		if errors.Is(readyCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("service readiness timed out after %s", timeout)
		}
		return readyCtx.Err()
	}
}

func runTask(ctx context.Context, task project.Task, rt *project.Runtime) error {
	if task.Run == nil {
		return nil
	}
	return task.Run(ctx, rt)
}

func (e *Engine) waitForPromptAnswer(ctx context.Context, req Request, instanceID, task string, prompt process.PromptRequest) (process.PromptResponse, error) {
	e.publish(api.Event{
		TS:         process.NowRFC3339Nano(),
		Type:       api.EventInteractionReq,
		InstanceID: instanceID,
		Worktree:   req.Worktree,
		Target:     req.Target,
		Task:       task,
		Mode:       req.Mode,
		PromptID:   prompt.ID,
		PromptKind: string(prompt.Kind),
		Prompt:     prompt.Prompt,
	})
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			e.publish(api.Event{
				TS:         process.NowRFC3339Nano(),
				Type:       api.EventInteractionStop,
				InstanceID: instanceID,
				Worktree:   req.Worktree,
				Target:     req.Target,
				Task:       task,
				Mode:       req.Mode,
				PromptID:   prompt.ID,
				PromptKind: string(prompt.Kind),
				Prompt:     prompt.Prompt,
				Error:      ctx.Err().Error(),
			})
			return process.PromptResponse{}, ctx.Err()
		case <-ticker.C:
			value, ok, err := instance.ConsumeInteractionAnswer(req.Worktree, instanceID, prompt.ID)
			if err != nil {
				return process.PromptResponse{}, err
			}
			if !ok {
				continue
			}
			e.publish(api.Event{
				TS:         process.NowRFC3339Nano(),
				Type:       api.EventInteractionAck,
				InstanceID: instanceID,
				Worktree:   req.Worktree,
				Target:     req.Target,
				Task:       task,
				Mode:       req.Mode,
				PromptID:   prompt.ID,
				PromptKind: string(prompt.Kind),
				Prompt:     prompt.Prompt,
			})
			return process.PromptResponse{Value: value}, nil
		}
	}
}

func (e *Engine) taskKey(ctx context.Context, rt *project.Runtime, task project.Task, depKeys []string) (string, error) {
	if task.CacheKeyOverride != nil {
		value, err := task.CacheKeyOverride(ctx, rt)
		if err != nil {
			return "", err
		}
		return fingerprint.TaskKey(fingerprint.TaskKeyInput{
			Task:     task,
			Override: value,
		})
	}
	inputHashes, envValues, custom, err := fingerprint.CollectTaskInputs(ctx, rt.Worktree, task, rt)
	if err != nil {
		return "", err
	}
	return fingerprint.TaskKey(fingerprint.TaskKeyInput{
		Task:               task,
		DepKeys:            depKeys,
		InputHashes:        inputHashes,
		EnvValues:          envValues,
		CustomFingerprints: custom,
	})
}

func (e *Engine) waitForServices(parent context.Context, req Request, inst *api.Instance, status map[string]api.NodeStatus, services map[string]*process.Handle) error {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	type exit struct {
		task string
		err  error
	}
	exits := make(chan exit, len(services))
	for name, handle := range services {
		go func(task string, h *process.Handle) {
			exits <- exit{task: task, err: h.Wait()}
		}(name, handle)
	}

	select {
	case <-ctx.Done():
		for _, name := range sortedHandles(services) {
			_ = services[name].Stop()
			node := status[name]
			prev := node.State
			node.State = api.StateStopped
			status[name] = node
			e.publish(api.Event{
				TS:            process.NowRFC3339Nano(),
				Type:          api.EventTaskState,
				InstanceID:    inst.ID,
				Worktree:      req.Worktree,
				Target:        req.Target,
				Task:          name,
				Mode:          req.Mode,
				State:         api.StateStopped,
				PreviousState: prev,
				PID:           node.PID,
			})
			e.publish(api.Event{
				TS:         process.NowRFC3339Nano(),
				Type:       api.EventProcessExited,
				InstanceID: inst.ID,
				Worktree:   req.Worktree,
				Target:     req.Target,
				Task:       name,
				Mode:       req.Mode,
				PID:        node.PID,
			})
		}
		return ctx.Err()
	case ex := <-exits:
		node := status[ex.task]
		prev := node.State
		if ex.err != nil {
			node.State = classifyTaskError(ctx, ex.err)
			node.LastError = displayTaskError(ctx, ex.err)
		} else {
			node.State = api.StateStopped
		}
		status[ex.task] = node
		_ = instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, status)
		e.publish(api.Event{
			TS:            process.NowRFC3339Nano(),
			Type:          api.EventTaskState,
			InstanceID:    inst.ID,
			Worktree:      req.Worktree,
			Target:        req.Target,
			Task:          ex.task,
			Mode:          req.Mode,
			State:         node.State,
			PreviousState: prev,
			PID:           node.PID,
			Error:         node.LastError,
		})
		e.publish(api.Event{
			TS:         process.NowRFC3339Nano(),
			Type:       api.EventProcessExited,
			InstanceID: inst.ID,
			Worktree:   req.Worktree,
			Target:     req.Target,
			Task:       ex.task,
			Mode:       req.Mode,
			PID:        node.PID,
			Error:      node.LastError,
		})
		for task, handle := range services {
			if task == ex.task {
				continue
			}
			_ = handle.Stop()
			node := status[task]
			prev := node.State
			node.State = api.StateStopped
			status[task] = node
			e.publish(api.Event{
				TS:            process.NowRFC3339Nano(),
				Type:          api.EventTaskState,
				InstanceID:    inst.ID,
				Worktree:      req.Worktree,
				Target:        req.Target,
				Task:          task,
				Mode:          req.Mode,
				State:         api.StateStopped,
				PreviousState: prev,
				PID:           node.PID,
			})
			e.publish(api.Event{
				TS:         process.NowRFC3339Nano(),
				Type:       api.EventProcessExited,
				InstanceID: inst.ID,
				Worktree:   req.Worktree,
				Target:     req.Target,
				Task:       task,
				Mode:       req.Mode,
				PID:        node.PID,
			})
		}
		if ex.err != nil {
			return fmt.Errorf("service %q exited: %w", ex.task, ex.err)
		}
		return nil
	}
}

func (s *runState) setNodeState(name string, state api.NodeState, lastRunKey, lastError string, pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.status[name]
	prev := node.State
	prevPID := node.PID
	prevError := node.LastError
	node.State = state
	if lastRunKey != "" {
		node.LastRunKey = lastRunKey
	}
	if lastError == "" {
		node.LastError = ""
	} else {
		node.LastError = lastError
	}
	if pid != 0 {
		node.PID = pid
	}
	s.status[name] = node
	s.saveLocked()
	if s.publish != nil && (prev != node.State || prevPID != node.PID || prevError != node.LastError) {
		s.publish(api.Event{
			TS:            process.NowRFC3339Nano(),
			Type:          api.EventTaskState,
			InstanceID:    s.inst.ID,
			Worktree:      s.req.Worktree,
			Target:        s.req.Target,
			Task:          name,
			Mode:          s.req.Mode,
			State:         node.State,
			PreviousState: prev,
			CacheKey:      node.LastRunKey,
			PID:           node.PID,
			Error:         node.LastError,
		})
	}
}

func (s *runState) setErrorState(name string, ctx context.Context, lastRunKey string, err error, pid int) {
	s.setNodeState(name, classifyTaskError(ctx, err), lastRunKey, displayTaskError(ctx, err), pid)
}

func classifyTaskError(ctx context.Context, err error) api.NodeState {
	if err == nil {
		return api.StateDone
	}
	if errors.Is(err, context.Canceled) || (ctx != nil && errors.Is(ctx.Err(), context.Canceled)) {
		return api.StateCanceled
	}
	return api.StateFailed
}

func displayTaskError(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) || (ctx != nil && errors.Is(ctx.Err(), context.Canceled)) {
		return "canceled"
	}
	return err.Error()
}

func (s *runState) setLastRunKey(name, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.status[name]
	node.LastRunKey = key
	s.status[name] = node
	s.saveLocked()
}

func (s *runState) setDepKey(name, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.depKeys[name] = key
}

func (s *runState) addCacheHit(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheHits = append(s.cacheHits, name)
}

func (s *runState) publishEvent(evt api.Event) {
	if s.publish == nil {
		return
	}
	s.publish(evt)
}

func (s *runState) registerService(task string, handle *process.Handle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.services[task] = handle
	s.inst.Processes[task] = api.ProcessRef{PID: handle.PID(), StartedAt: time.Now().UTC()}
	node := s.status[task]
	node.PID = handle.PID()
	s.status[task] = node
	_ = instance.Save(s.inst)
	s.saveLocked()
}

func (s *runState) serviceHandle(task string) (*process.Handle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	handle, ok := s.services[task]
	return handle, ok
}

func (s *runState) removeService(task string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.services, task)
	delete(s.inst.Processes, task)
	node := s.status[task]
	node.PID = 0
	s.status[task] = node
	_ = instance.Save(s.inst)
	s.saveLocked()
}

func (s *runState) depKeySnapshot(deps []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(deps))
	for _, dep := range deps {
		if key := s.depKeys[dep]; key != "" {
			out = append(out, key)
		}
	}
	return out
}

func (s *runState) snapshotCacheHits() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.cacheHits...)
	sort.Strings(out)
	return out
}

func (s *runState) snapshotServices() map[string]*process.Handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*process.Handle, len(s.services))
	for name, handle := range s.services {
		out[name] = handle
	}
	return out
}

func (s *runState) stopServices(req Request, tasks []string) {
	for _, name := range tasks {
		task := name
		s.mu.Lock()
		handle, ok := s.services[task]
		node := s.status[task]
		if ok {
			delete(s.services, task)
			delete(s.inst.Processes, task)
		}
		s.mu.Unlock()
		if !ok {
			continue
		}
		_ = handle.Stop()
		prev := node.State
		s.setNodeState(task, api.StateStopped, node.LastRunKey, "", node.PID)
		s.publishEvent(api.Event{
			TS:            process.NowRFC3339Nano(),
			Type:          api.EventProcessExited,
			InstanceID:    s.inst.ID,
			Worktree:      req.Worktree,
			Target:        req.Target,
			Task:          task,
			Mode:          req.Mode,
			PID:           node.PID,
			State:         api.StateStopped,
			PreviousState: prev,
		})
	}
	_ = instance.Save(s.inst)
}

func (s *runState) statusSnapshot() map[string]api.NodeStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]api.NodeStatus, len(s.status))
	for name, node := range s.status {
		out[name] = node
	}
	return out
}

func (s *runState) failedNode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range sortedNodeNames(s.status) {
		if s.status[name].State == api.StateFailed {
			return name
		}
	}
	return ""
}

func (s *runState) saveLocked() {
	_ = instance.SaveStatus(s.req.Worktree, s.inst.ID, s.req.Target, s.req.Mode, s.status)
}

func normalizeMaxParallel(n int) int {
	if n > 0 {
		return n
	}
	if runtime.GOMAXPROCS(0) > 0 {
		return runtime.GOMAXPROCS(0)
	}
	return 1
}

func sortReadyQueue(ready []string, g *graph.Graph) {
	sort.Slice(ready, func(i, j int) bool {
		left := g.Tasks[ready[i]]
		right := g.Tasks[ready[j]]
		if left.Kind == project.KindWarmup && right.Kind != project.KindWarmup {
			return true
		}
		if left.Kind != project.KindWarmup && right.Kind == project.KindWarmup {
			return false
		}
		return ready[i] < ready[j]
	})
}

func mergeInstanceEnv(current map[string]string, overlays ...map[string]string) map[string]string {
	out := cloneMap(current)
	if out == nil {
		out = map[string]string{}
	}
	for _, overlay := range overlays {
		for key, value := range overlay {
			out[key] = value
		}
	}
	return out
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func sortedHandles(m map[string]*process.Handle) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedNodeNames(m map[string]api.NodeStatus) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedBoolKeys(m map[string]bool) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (e *Engine) affectedWatchOrder(target string, files []string) ([]string, []string) {
	closure, err := e.graph.TargetClosure(target)
	if err != nil {
		return nil, nil
	}
	inClosure := map[string]bool{}
	for _, name := range closure {
		inClosure[name] = true
	}
	direct := e.graph.AffectedByFiles(files)
	filteredDirect := make([]string, 0, len(direct))
	for _, name := range direct {
		if inClosure[name] {
			filteredDirect = append(filteredDirect, name)
		}
	}
	if len(filteredDirect) == 0 {
		return nil, nil
	}
	downstream := e.watchDownstream(filteredDirect)
	filtered := make([]string, 0, len(downstream))
	for _, name := range downstream {
		if !inClosure[name] {
			continue
		}
		task := e.graph.Tasks[name]
		if task.Kind == project.KindWarmup && !task.AllowInWatch {
			continue
		}
		if task.Kind == project.KindService && task.Restart == project.RestartNever {
			continue
		}
		filtered = append(filtered, name)
	}
	order, err := e.graph.TopoSort(filtered)
	if err != nil {
		return nil, filteredDirect
	}
	return order, filteredDirect
}

func (e *Engine) watchDownstream(names []string) []string {
	seen := map[string]bool{}
	queue := append([]string(nil), names...)
	reverse := map[string][]string{}
	for _, task := range e.graph.Tasks {
		for _, dep := range task.Deps {
			reverse[dep] = append(reverse[dep], task.Name)
		}
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		current := e.graph.Tasks[name]
		for _, child := range reverse[name] {
			if current.Kind == project.KindService && e.graph.Tasks[child].Kind == project.KindService && !e.graph.Tasks[child].WatchRestartOnServiceDeps {
				continue
			}
			queue = append(queue, child)
		}
	}
	return sortedBoolKeys(seen)
}

func (e *Engine) stopAllServices(req Request, inst *api.Instance, state *runState) {
	state.stopServices(req, sortedHandles(state.snapshotServices()))
}

func (e *Engine) publish(evt api.Event) {
	e.events.Publish(evt)
}

func (e *Engine) publishRunFinished(result api.RunResult, worktree, errText string) {
	success := result.Success
	e.publish(api.Event{
		TS:         process.NowRFC3339Nano(),
		Type:       api.EventRunFinished,
		InstanceID: result.InstanceID,
		Worktree:   worktree,
		Target:     result.Target,
		Mode:       result.Mode,
		Task:       result.FailedNode,
		Error:      errText,
		Success:    &success,
	})
}

func boolPtr(v bool) *bool {
	return &v
}
