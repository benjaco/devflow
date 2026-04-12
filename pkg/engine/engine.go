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
	logs    event.Bus[api.LogEvent]
}

type runState struct {
	mu        sync.Mutex
	req       Request
	inst      *api.Instance
	status    map[string]api.NodeStatus
	depKeys   map[string]string
	cacheHits []string
	services  map[string]*process.Handle
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

func (e *Engine) Run(ctx context.Context, req Request) (*Outcome, error) {
	started := time.Now().UTC()
	inst, err := instance.Resolve(req.Worktree, filepath.Base(req.Worktree))
	if err != nil {
		return nil, err
	}

	cfg, err := e.project.ConfigureInstance(ctx, req.Worktree)
	if err != nil {
		return nil, err
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
			return nil, err
		}
	}
	if err := instance.Save(inst); err != nil {
		return nil, err
	}

	order, err := e.graph.TargetClosure(req.Target)
	if err != nil {
		return nil, err
	}

	state := &runState{
		req:      req,
		inst:     inst,
		status:   map[string]api.NodeStatus{},
		depKeys:  map[string]string{},
		services: map[string]*process.Handle{},
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

	baseRT := &project.Runtime{
		Worktree: req.Worktree,
		Instance: inst,
		Mode:     req.Mode,
		Env:      cloneMap(inst.Env),
		EventFn: func(evt api.LogEvent) {
			e.logs.Publish(evt)
		},
		OnService: func(task string, handle *process.Handle) {
			state.registerService(task, handle)
		},
	}

	if err := e.runReadyQueue(runCtx, cancel, baseRT, state, order); err != nil {
		result.FailedNode = state.failedNode()
		result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		result.DurationMs = time.Since(started).Milliseconds()
		return &Outcome{Result: result, Instance: inst}, err
	}

	result.Success = true
	result.CacheHits = state.snapshotCacheHits()

	services := state.snapshotServices()
	if len(services) > 0 && req.Mode != api.ModeCI {
		waitErr := e.waitForServices(ctx, req, inst, state.statusSnapshot(), services)
		if waitErr != nil && !errors.Is(waitErr, context.Canceled) {
			return nil, waitErr
		}
	}

	result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	result.DurationMs = time.Since(started).Milliseconds()
	if err := instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, state.statusSnapshot()); err != nil {
		return nil, err
	}
	return &Outcome{Result: result, Instance: inst}, nil
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
			state.setNodeState(name, api.StateRunning, "", "", 0)

			depKeys := state.depKeySnapshot(task.Deps)
			rt := baseRT.WithTask(name, instance.LogPath(state.req.Worktree, state.inst.ID, name))
			wg.Add(1)
			running++
			go func(task project.Task, taskName string, depSnapshot []string, runtime *project.Runtime) {
				defer wg.Done()
				results <- e.executeTask(ctx, state, runtime, task, depSnapshot)
			}(task, name, depKeys, rt)
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
			state.setNodeState(task.Name, api.StateFailed, "", err.Error(), 0)
			return taskResult{name: task.Name, err: err}
		}
		state.setLastRunKey(task.Name, key)
		if ok, restoreErr := e.cache.Restore(rt.Worktree, task.Name, key); restoreErr == nil && ok {
			state.setNodeState(task.Name, api.StateCached, key, "", 0)
			return taskResult{name: task.Name, key: key, cached: true}
		}
		if err := runTask(ctx, task, rt); err != nil {
			state.setNodeState(task.Name, api.StateFailed, key, err.Error(), 0)
			return taskResult{name: task.Name, key: key, err: err}
		}
		if _, err := e.cache.Snapshot(rt.Worktree, task, key); err != nil {
			state.setNodeState(task.Name, api.StateFailed, key, err.Error(), 0)
			return taskResult{name: task.Name, key: key, err: err}
		}
		state.setNodeState(task.Name, api.StateDone, key, "", 0)
		return taskResult{name: task.Name, key: key}
	}

	if err := runTask(ctx, task, rt); err != nil {
		state.setNodeState(task.Name, api.StateFailed, "", err.Error(), 0)
		return taskResult{name: task.Name, err: err}
	}
	if task.Kind == project.KindService {
		if !state.hasService(task.Name) {
			err := fmt.Errorf("service task %q returned without starting a service", task.Name)
			state.setNodeState(task.Name, api.StateFailed, "", err.Error(), 0)
			return taskResult{name: task.Name, err: err}
		}
		return taskResult{name: task.Name}
	}
	state.setNodeState(task.Name, api.StateDone, "", "", 0)
	return taskResult{name: task.Name}
}

func runTask(ctx context.Context, task project.Task, rt *project.Runtime) error {
	if task.Run == nil {
		return nil
	}
	return task.Run(ctx, rt)
}

func (e *Engine) taskKey(ctx context.Context, rt *project.Runtime, task project.Task, depKeys []string) (string, error) {
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
			node.State = api.StateStopped
			status[name] = node
		}
		return ctx.Err()
	case ex := <-exits:
		node := status[ex.task]
		if ex.err != nil {
			node.State = api.StateFailed
			node.LastError = ex.err.Error()
		} else {
			node.State = api.StateStopped
		}
		status[ex.task] = node
		_ = instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, status)
		for task, handle := range services {
			if task == ex.task {
				continue
			}
			_ = handle.Stop()
			node := status[task]
			node.State = api.StateStopped
			status[task] = node
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

func (s *runState) registerService(task string, handle *process.Handle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.services[task] = handle
	s.inst.Processes[task] = api.ProcessRef{PID: handle.PID(), StartedAt: time.Now().UTC()}
	node := s.status[task]
	node.PID = handle.PID()
	node.State = api.StateRunning
	s.status[task] = node
	_ = instance.Save(s.inst)
	s.saveLocked()
}

func (s *runState) hasService(task string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.services[task]
	return ok
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
