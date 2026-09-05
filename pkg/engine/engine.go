package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/benjaco/devflow/internal/execution"
	"github.com/benjaco/devflow/internal/executionconflict"
	"github.com/benjaco/devflow/internal/executionstate"
	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/cache"
	"github.com/benjaco/devflow/pkg/event"
	"github.com/benjaco/devflow/pkg/fingerprint"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/ports"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
	"github.com/benjaco/devflow/pkg/watch"
)

type Request struct {
	Target               string
	Worktree             string
	Mode                 api.RunMode
	MaxParallel          int
	CacheKeyManifestPath string
	LifecycleController  *LifecycleController
	// WatchReady closes once the input baseline exists, before the initial DAG.
	WatchReady chan<- struct{}
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
	inputs  *fingerprint.FilteredContentCache
}

type runState struct {
	mu          sync.Mutex
	req         Request
	inst        *api.Instance
	status      map[string]api.NodeStatus
	depKeys     map[string]string
	cacheHits   []string
	cacheMisses []string
	nodeStarted map[string]time.Time
	services    map[string]project.ServiceHandle
	// serviceGeneration distinguishes a replaced handle from late Wait results
	// produced by the previous handle, including PID-less test/service handles.
	serviceGeneration map[string]uint64
	publish           func(api.Event)
	manifest          *validatedCacheKeyManifest
	manifestUsage     *api.CacheKeyManifestUsage
	redactDiagnostic  func(string) string
	watchOutputs      []watchOutputEvidence
	watchBlocked      map[string]bool
}

type taskResult struct {
	name   string
	key    string
	cached bool
	err    error
}

type serviceExit struct {
	task       string
	generation uint64
	err        error
}

func New(p project.Project, worktree string) (*Engine, error) {
	tasks := p.Tasks()
	g, err := graph.New(tasks, p.Targets())
	if err != nil {
		return nil, err
	}
	// Keep graph inspection and validation able to diagnose this contract,
	// but reject it before any execution or instance provisioning occurs.
	for _, task := range tasks {
		if task.Cache && len(task.Outputs.Paths)+len(task.Outputs.Files)+len(task.Outputs.Dirs) == 0 {
			return nil, fmt.Errorf("cacheable task %q must declare outputs", task.Name)
		}
	}
	pm, err := ports.NewDefaultForWorktree(worktree)
	if err != nil {
		return nil, err
	}
	return &Engine{
		project: p,
		graph:   g,
		cache:   cache.NewNamespaced(instance.CacheRoot(), project.CacheNamespace(p)),
		ports:   pm,
		inputs:  fingerprint.NewFilteredContentCache(),
	}, nil
}

func (e *Engine) Graph() *graph.Graph {
	return e.graph
}

func (e *Engine) SubscribeEvents() <-chan api.Event {
	return e.events.Subscribe()
}

// SubscribeEventsLossless returns a backpressured event subscription for
// consumers that must observe every event, such as direct CI progress output.
func (e *Engine) SubscribeEventsLossless() <-chan api.Event {
	return e.events.SubscribeLossless()
}

func (e *Engine) CacheKey(ctx context.Context, req Request) (*api.CacheKeyResult, error) {
	result, _, err := e.CacheKeyWithManifest(ctx, req)
	return result, err
}

func (e *Engine) Watch(ctx context.Context, req Request) (runErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lease, release, err := acquireExecution(ctx, req)
	if err != nil {
		return err
	}
	defer release()
	if req.LifecycleController != nil {
		defer req.LifecycleController.closeController()
	}
	started := time.Now().UTC()
	inst, state, baseRT, err := e.prepareExecution(ctx, req)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := state.stopServices(req, sortedHandles(state.snapshotServices())); cleanupErr != nil {
			lease.RequireRecovery()
			runErr = errors.Join(runErr, cleanupErr)
		}
		result := api.RunResult{
			Target: req.Target, Mode: req.Mode, InstanceID: inst.ID,
			Success: runErr == nil, DurationMs: time.Since(started).Milliseconds(),
			StartedAt: started.Format(time.RFC3339), FinishedAt: time.Now().UTC().Format(time.RFC3339),
		}
		finalizeRunResult(&result, state, runErr)
		e.publishRunFinished(result, req.Worktree, result.Error)
	}()
	e.publish(api.Event{
		TS: process.NowRFC3339Nano(), Type: api.EventRunStarted,
		InstanceID: inst.ID, Worktree: req.Worktree, Target: req.Target, Mode: req.Mode,
	})
	order, err := e.graph.TargetClosure(req.Target)
	if err != nil {
		return err
	}
	flushSyncDir := instance.FlushSyncDir(req.Worktree, inst.ID)
	runner, err := watch.New(watch.Options{
		Root: req.Worktree, WatchPaths: e.watchInputPaths(order),
		WatchOnly: true, IncludePaths: []string{flushSyncDir},
	})
	if err != nil {
		return err
	}
	// Baseline first: a task may read an input long before startup completes.
	batches, errs, err := runner.Start(ctx)
	if err != nil {
		return err
	}
	watchErrors := make(chan error, 1)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		select {
		case err, ok := <-errs:
			if ok && err != nil {
				if err == ctx.Err() {
					return
				}
				watchErrors <- err
				cancel()
			}
		case <-ctx.Done():
		}
	}()
	defer func() {
		cancel()
		<-monitorDone
		// Only a canceled observer wait is normal shutdown; wrapped stop errors
		// must remain failures even if final cleanup subsequently succeeds.
		if runErr == context.Canceled {
			runErr = nil
		}
		select {
		case err := <-watchErrors:
			runErr = errors.Join(runErr, err)
		default:
		}
	}()
	if req.WatchReady != nil {
		close(req.WatchReady)
	}

	initialErr := e.runReadyQueue(ctx, func() {}, baseRT, state, order)
	e.publish(api.Event{
		TS: process.NowRFC3339Nano(), Type: api.EventWatchCycleDone,
		InstanceID: inst.ID, Worktree: req.Worktree, Target: req.Target, Mode: req.Mode,
		Success: boolPtr(initialErr == nil),
	})
	serviceExits := make(chan serviceExit, len(e.graph.Tasks)+1)
	watchedServices := map[string]uint64{}
	watchServiceHandles(state, serviceExits, watchedServices)
	var lifecycleCommands <-chan serviceLifecycleCommand
	if req.LifecycleController != nil {
		lifecycleCommands = req.LifecycleController.commands
	}
	reconcile := func(batch watch.Batch) error {
		return e.reconcileWatch(ctx, req, baseRT, state, runner, batch, func() {
			watchServiceHandles(state, serviceExits, watchedServices)
		})
	}
	if err := reconcile(watch.Batch{}); err != nil {
		return err
	}
	readyPath := instance.FlushWatchReadyPath(req.Worktree, inst.ID)
	if err := os.WriteFile(readyPath, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		return err
	}
	defer os.Remove(readyPath)
	for {
		select {
		case <-ctx.Done():
			return nil
		case command := <-lifecycleCommands:
			result, commandErr := e.applyServiceLifecycleCommand(ctx, req, state, baseRT, command)
			command.result <- serviceLifecycleResponse{result: result, err: commandErr}
			watchServiceHandles(state, serviceExits, watchedServices)
			if err := reconcile(watch.Batch{}); err != nil {
				return err
			}
		case exited := <-serviceExits:
			e.handleUnexpectedServiceExit(ctx, req, inst, state, exited)
		case batch, ok := <-batches:
			if !ok {
				return nil
			}
			if err := reconcile(batch); err != nil {
				return err
			}
		}
	}
}

func (e *Engine) Run(ctx context.Context, req Request) (*Outcome, error) {
	if req.LifecycleController != nil {
		defer req.LifecycleController.closeController()
	}
	started := time.Now().UTC()
	result := api.RunResult{
		Target: req.Target, Mode: req.Mode, FailureExcerpts: []api.FailureExcerpt{},
		Nodes: []api.NodeStatus{}, CacheHits: []string{}, CacheMisses: []string{},
		StartedAt: started.Format(time.RFC3339),
	}
	failAdmission := func(err error) (*Outcome, error) {
		result.Error = err.Error()
		result.ResourceConflict = executionconflict.Details(err)
		if result.ResourceConflict != nil {
			result.Code = "resource_conflict"
		}
		result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		result.DurationMs = time.Since(started).Milliseconds()
		return &Outcome{Result: result}, err
	}
	lease, release, err := acquireExecution(ctx, req)
	if err != nil {
		return failAdmission(err)
	}
	defer release()
	inst, state, baseRT, err := e.prepareExecution(ctx, req)
	if err != nil {
		var manifestErr *CacheKeyManifestError
		if errors.As(err, &manifestErr) {
			result.CacheKeyManifest = &api.CacheKeyManifestUsage{Validated: false, Error: manifestErr.Error(), ValidationDurationMs: manifestErr.DurationMs, ReusedTasks: []string{}, LocalInputChangedTasks: []string{}}
			return failAdmission(err)
		}
		return nil, err
	}
	result.InstanceID = inst.ID
	order, err := e.graph.TargetClosure(req.Target)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.publish(api.Event{TS: process.NowRFC3339Nano(), Type: api.EventRunStarted, InstanceID: inst.ID, Worktree: req.Worktree, Target: req.Target, Mode: req.Mode})
	runErr := e.runReadyQueue(runCtx, cancel, baseRT, state, order)
	if runErr == nil && req.Mode != api.ModeCI {
		if services := state.snapshotServices(); len(services) > 0 {
			runErr = e.waitForServices(ctx, req, inst, state, baseRT, services)
			if errors.Is(runErr, context.Canceled) {
				runErr = nil
			}
		}
	}
	// A return from execution is not proof of cleanup. Keep failed handles and
	// ownership evidence until every registered resource confirms it stopped.
	if cleanupErr := state.stopServices(req, sortedHandles(state.snapshotServices())); cleanupErr != nil {
		lease.RequireRecovery()
		runErr = errors.Join(runErr, cleanupErr)
	}
	result.Success = runErr == nil
	result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	result.DurationMs = time.Since(started).Milliseconds()
	finalizeRunResult(&result, state, runErr)
	if err := instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, state.statusSnapshot()); err != nil {
		runErr = errors.Join(runErr, err)
		result.Success = false
		result.Error = runErr.Error()
	}
	e.publishRunFinished(result, req.Worktree, result.Error)
	return &Outcome{Result: result, Instance: inst}, runErr
}

// acquireExecution borrows admission only from the enclosing operation that
// owns this exact worktree. Direct engine consumers receive the same guard.
func acquireExecution(ctx context.Context, req Request) (*execution.Lease, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, func() {}, err
	}
	if lease := execution.FromContext(ctx); lease != nil && lease.ValidFor(req.Worktree) {
		return lease, func() {}, nil
	}
	lease, err := executionstate.Acquire(req.Worktree, execution.Owner{Target: req.Target, Mode: string(req.Mode), Kind: "engine"})
	if err != nil {
		return nil, func() {}, err
	}
	return lease, func() { _ = lease.Release() }, nil
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
	inst.Env = mergeInstanceEnv(inst.Env, cfg.Env, selectedProcessEnv(e.project, cfg.Env, inst.Env), map[string]string{
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
	order, err := e.graph.TargetClosure(req.Target)
	if err != nil {
		return nil, nil, nil, err
	}
	var manifest *validatedCacheKeyManifest
	var manifestUsage *api.CacheKeyManifestUsage
	if req.CacheKeyManifestPath != "" {
		manifestStarted := time.Now()
		manifest, err = e.loadAndValidateCacheKeyManifest(req.CacheKeyManifestPath, req, inst, order)
		manifestDuration := elapsedMilliseconds(manifestStarted)
		if err != nil {
			var manifestErr *CacheKeyManifestError
			if errors.As(err, &manifestErr) {
				manifestErr.DurationMs = manifestDuration
			}
			return nil, nil, nil, err
		}
		manifestUsage = &api.CacheKeyManifestUsage{
			Validated:              true,
			ValidationDurationMs:   manifestDuration,
			ReusedTasks:            []string{},
			LocalInputChangedTasks: []string{},
		}
	}
	if err := instance.Save(inst); err != nil {
		return nil, nil, nil, err
	}

	state := &runState{
		req:               req,
		inst:              inst,
		status:            map[string]api.NodeStatus{},
		depKeys:           map[string]string{},
		nodeStarted:       map[string]time.Time{},
		services:          map[string]project.ServiceHandle{},
		serviceGeneration: map[string]uint64{},
		publish:           e.publish,
		manifest:          manifest,
		manifestUsage:     manifestUsage,
		redactDiagnostic:  diagnosticRedactor(inst),
	}
	for _, name := range order {
		task := e.graph.Tasks[name]
		state.status[name] = api.NodeStatus{
			Name:    name,
			Kind:    string(task.Kind),
			State:   api.StatePending,
			LogPath: instance.LogPath(req.Worktree, inst.ID, name),
			Debug:   debugStatus(task, inst),
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
		OnServiceHandle: func(task string, handle project.ServiceHandle) {
			state.registerService(task, handle)
		},
		OnPrompt: func(task string, prompt process.PromptRequest) (process.PromptResponse, error) {
			return e.waitForPromptAnswer(ctx, req, inst.ID, task, prompt)
		},
	}
	return inst, state, baseRT, nil
}

func debugStatus(task project.Task, inst *api.Instance) *api.DebugStatus {
	if task.Debug == nil || inst == nil {
		return nil
	}
	port := 0
	if task.Debug.PortName != "" {
		port = inst.Ports[task.Debug.PortName]
	}
	host := task.Debug.Host
	if host == "" {
		host = "127.0.0.1"
	}
	protocol := task.Debug.Protocol
	if protocol == "" {
		protocol = "dap"
	}
	debugType := task.Debug.Type
	if debugType == "" {
		debugType = "debug"
	}
	attachType := debugType
	if debugType == "go" {
		attachType = "go"
	}
	attachName := "Attach Devflow: " + task.Name
	return &api.DebugStatus{
		Type:     debugType,
		Host:     host,
		Port:     port,
		PortName: task.Debug.PortName,
		Protocol: protocol,
		Binary:   task.Debug.Binary,
		Package:  task.Debug.Package,
		Attach: api.DebugAttachConfig{
			Name:         attachName,
			Type:         attachType,
			Request:      "attach",
			Mode:         "remote",
			Host:         host,
			Port:         port,
			DebugAdapter: "dlv-dap",
			CWD:          inst.Worktree,
		},
	}
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
	failedTask := ""

	for completed < len(order) {
		for !failed && running < maxParallel && len(ready) > 0 {
			sortReadyQueue(ready, e.graph)
			name := ready[0]
			ready = ready[1:]
			task := e.graph.Tasks[name]

			if task.Kind == project.KindGroup {
				state.setNodeState(name, api.StateDone, "", "", 0)
				state.clearWatchBlocked(name)
				for _, child := range dependents[name] {
					pendingDeps[child]--
					if pendingDeps[child] == 0 {
						ready = append(ready, child)
					}
				}
				completed++
				continue
			}

			if project.IsServiceKind(task.Kind) {
				state.setNodeState(name, api.StateStarting, "", "", 0)
			} else {
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
			if runErr == nil {
				runErr = res.err
				failedTask = res.name
			}
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
	if failedTask != "" {
		// The scheduler stops launching new work after the first failure. Mark
		// the untouched nodes now so status consumers can distinguish a real
		// dependency block from work that simply has not started yet.
		downstream := make(map[string]bool)
		for _, name := range e.graph.Downstream([]string{failedTask}) {
			downstream[name] = true
		}
		for _, name := range order {
			node := state.statusSnapshot()[name]
			if node.State != api.StatePending && node.State != api.StateStarting {
				continue
			}
			if downstream[name] {
				state.setNodeState(name, api.StateBlocked, node.LastRunKey, fmt.Sprintf("blocked by failed dependency %s", failedTask), 0)
			} else {
				state.setNodeState(name, api.StateCanceled, node.LastRunKey, fmt.Sprintf("canceled after %s failed", failedTask), 0)
			}
		}
	}
	return runErr
}

func (e *Engine) executeTask(ctx context.Context, state *runState, rt *project.Runtime, task project.Task, depKeys []string) (result taskResult) {
	var finishOutputs func() watchOutputEvidence
	beginOutputs := func() {
		if state.req.Mode == api.ModeWatch && finishOutputs == nil {
			finishOutputs = beginWatchOutputs(ctx, rt.Worktree, task.Outputs)
		}
	}
	defer func() {
		if finishOutputs != nil {
			state.recordWatchOutputs(finishOutputs())
		}
		if result.err == nil {
			state.clearWatchBlocked(task.Name)
		}
	}()
	if err := truncateTaskLog(rt); err != nil {
		state.setErrorState(task.Name, ctx, "", err, 0)
		return taskResult{name: task.Name, err: err}
	}
	if task.Stamp {
		// Stamps are local install/setup markers; never use the global cache to skip or restore them.
		computation, err := e.computeTaskKey(ctx, rt, task, depKeys, state.manifest)
		if err != nil {
			state.setErrorState(task.Name, ctx, "", err, 0)
			return taskResult{name: task.Name, err: err}
		}
		key := computation.key
		state.recordManifestComputation(task.Name, computation)
		state.setLastRunKey(task.Name, key)
		if stamp, ok, loadErr := instance.LoadTaskStamp(rt.Worktree, state.inst.ID, task.Name); loadErr != nil {
			state.setErrorState(task.Name, ctx, key, loadErr, 0)
			return taskResult{name: task.Name, key: key, err: loadErr}
		} else if ok && stamp.Key == key {
			outputsExist, outputErr := declaredOutputsExist(rt.Worktree, task.Outputs)
			if outputErr != nil {
				state.setErrorState(task.Name, ctx, key, outputErr, 0)
				return taskResult{name: task.Name, key: key, err: outputErr}
			}
			if outputsExist {
				state.setNodeState(task.Name, api.StateDone, key, "", 0)
				return taskResult{name: task.Name, key: key}
			}
		}
		beginOutputs()
		if _, err := runTask(ctx, task, rt); err != nil {
			state.setErrorState(task.Name, ctx, key, err, 0)
			return taskResult{name: task.Name, key: key, err: err}
		}
		if err := instance.WriteTaskStamp(rt.Worktree, state.inst.ID, task.Name, key); err != nil {
			state.setErrorState(task.Name, ctx, key, err, 0)
			return taskResult{name: task.Name, key: key, err: err}
		}
		state.setNodeState(task.Name, api.StateDone, key, "", 0)
		return taskResult{name: task.Name, key: key}
	}

	if task.Cache {
		keyStarted := time.Now()
		computation, err := e.computeTaskKey(ctx, rt, task, depKeys, state.manifest)
		keyDuration := elapsedMilliseconds(keyStarted)
		if err != nil {
			state.setErrorState(task.Name, ctx, "", err, 0)
			return taskResult{name: task.Name, err: err}
		}
		key := computation.key
		state.recordManifestComputation(task.Name, computation)
		manifestDuration, manifestComponents := state.manifestTiming(computation)
		state.setLastRunKey(task.Name, key)
		restoreStarted := time.Now()
		beginOutputs()
		ok, restoreErr := e.cache.RestoreContext(ctx, rt.Worktree, task.Name, key, cacheCopyProgress(rt, "restore"))
		readDuration := elapsedMilliseconds(restoreStarted)
		if restoreErr != nil {
			state.setErrorState(task.Name, ctx, key, restoreErr, 0)
			return taskResult{name: task.Name, key: key, err: restoreErr}
		}
		if ok {
			state.setCacheTiming(task.Name, api.CacheTiming{
				Outcome:                        "hit",
				KeyDurationMs:                  keyDuration,
				ReadDurationMs:                 readDuration,
				ManifestValidationMs:           manifestDuration,
				ManifestComponents:             manifestComponents,
				LocalInputsChangedFromManifest: computation.localInputsChanged,
				TotalDurationMs:                keyDuration + readDuration + manifestDuration,
			})
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
		state.addCacheMiss(task.Name)
		state.setCacheTiming(task.Name, api.CacheTiming{
			Outcome:                        "miss",
			KeyDurationMs:                  keyDuration,
			ReadDurationMs:                 readDuration,
			ManifestValidationMs:           manifestDuration,
			ManifestComponents:             manifestComponents,
			LocalInputsChangedFromManifest: computation.localInputsChanged,
			TotalDurationMs:                keyDuration + readDuration + manifestDuration,
		})
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
		beginOutputs()
		if _, err := runTask(ctx, task, rt); err != nil {
			state.setErrorState(task.Name, ctx, key, err, 0)
			return taskResult{name: task.Name, key: key, err: err}
		}
		writeStarted := time.Now()
		if _, err := e.cache.SnapshotContext(ctx, rt.Worktree, task, key, cacheCopyProgress(rt, "snapshot")); err != nil {
			state.setErrorState(task.Name, ctx, key, err, 0)
			return taskResult{name: task.Name, key: key, err: err}
		}
		writeDuration := elapsedMilliseconds(writeStarted)
		state.setCacheTiming(task.Name, api.CacheTiming{
			Outcome:                        "miss",
			KeyDurationMs:                  keyDuration,
			ReadDurationMs:                 readDuration,
			WriteDurationMs:                writeDuration,
			ManifestValidationMs:           manifestDuration,
			ManifestComponents:             manifestComponents,
			LocalInputsChangedFromManifest: computation.localInputsChanged,
			TotalDurationMs:                keyDuration + readDuration + writeDuration + manifestDuration,
		})
		state.setNodeState(task.Name, api.StateDone, key, "", 0)
		return taskResult{name: task.Name, key: key}
	}

	beginOutputs()
	taskRuntime, err := runTask(ctx, task, rt)
	if err != nil {
		if project.IsServiceKind(task.Kind) {
			err = errors.Join(err, state.stopServices(state.req, []string{task.Name}))
		}
		state.setErrorState(task.Name, ctx, "", err, 0)
		return taskResult{name: task.Name, err: err}
	}
	if project.IsServiceKind(task.Kind) {
		handle, ok := state.serviceHandle(task.Name)
		if !ok {
			err := fmt.Errorf("service task %q returned without starting a service", task.Name)
			state.setErrorState(task.Name, ctx, "", err, 0)
			return taskResult{name: task.Name, err: err}
		}
		if err := e.awaitServiceReady(ctx, taskRuntime, task, handle); err != nil {
			err = errors.Join(err, state.stopServices(state.req, []string{task.Name}))
			state.setErrorState(task.Name, ctx, "", err, 0)
			return taskResult{name: task.Name, err: err}
		}
		state.setNodeState(task.Name, api.StateRunning, "", "", handle.PID())
		state.setNodeReady(task.Name, true)
		return taskResult{name: task.Name}
	}
	state.setNodeState(task.Name, api.StateDone, "", "", 0)
	return taskResult{name: task.Name}
}

func (e *Engine) awaitServiceReady(ctx context.Context, rt *project.Runtime, task project.Task, handle project.ServiceHandle) error {
	if task.Ready != nil {
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
			if err := readyCtx.Err(); err != nil {
				return err
			}
			// A successful probe can race the process exit. Do not commit
			// AfterReady state for a service that is already known to be dead.
			select {
			case err := <-exitCh:
				return &serviceEarlyExitError{cause: err}
			default:
			}
			if !handle.Alive() {
				return &serviceEarlyExitError{}
			}
		case err := <-exitCh:
			return &serviceEarlyExitError{cause: err}
		case <-readyCtx.Done():
			if errors.Is(readyCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("service readiness timed out after %s", timeout)
			}
			return readyCtx.Err()
		}
	}
	if task.AfterReady != nil {
		return task.AfterReady(ctx, rt)
	}
	return nil
}

type serviceEarlyExitError struct {
	cause error
}

func (e *serviceEarlyExitError) Error() string {
	if e != nil && e.cause != nil {
		return fmt.Sprintf("service exited before readiness: %v", e.cause)
	}
	return "service exited before readiness"
}

func (e *serviceEarlyExitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func runTask(ctx context.Context, task project.Task, rt *project.Runtime) (*project.Runtime, error) {
	taskRuntime := rt
	if task.BeforeRun != nil {
		clone := *rt
		clone.Env = rt.CloneEnv()
		taskRuntime = &clone
		if err := task.BeforeRun(ctx, taskRuntime); err != nil {
			return taskRuntime, err
		}
	}
	if task.Run == nil {
		return taskRuntime, nil
	}
	return taskRuntime, task.Run(ctx, taskRuntime)
}

func truncateTaskLog(rt *project.Runtime) error {
	if rt == nil || rt.LogPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(rt.LogPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(rt.LogPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func cacheCopyProgress(rt *project.Runtime, operation string) func(fsutil.CopyProgress) {
	if rt == nil {
		return nil
	}
	var lastFiles, lastBytes int64
	return func(progress fsutil.CopyProgress) {
		if !progress.Done && progress.Files-lastFiles < 1000 && progress.Bytes-lastBytes < 100<<20 {
			return
		}
		lastFiles = progress.Files
		lastBytes = progress.Bytes
		rt.EmitLogLine("stderr", fmt.Sprintf("cache %s: files=%d bytes=%d", operation, progress.Files, progress.Bytes))
	}
}

func elapsedMilliseconds(started time.Time) int64 {
	return durationMilliseconds(time.Since(started))
}

func declaredOutputsExist(worktree string, outputs project.Outputs) (bool, error) {
	for _, rel := range outputs.Paths {
		if _, err := os.Stat(filepath.Join(worktree, rel)); err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
	}
	for _, rel := range outputs.Files {
		info, err := os.Stat(filepath.Join(worktree, rel))
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		if info.IsDir() {
			return false, nil
		}
	}
	for _, rel := range outputs.Dirs {
		info, err := os.Stat(filepath.Join(worktree, rel))
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		if !info.IsDir() {
			return false, nil
		}
	}
	return true, nil
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

func (e *Engine) waitForServices(parent context.Context, req Request, inst *api.Instance, state *runState, baseRT *project.Runtime, services map[string]project.ServiceHandle) error {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	exits := make(chan serviceExit, len(services)+1)
	watched := map[string]uint64{}
	watchServiceHandles(state, exits, watched)

	var commands <-chan serviceLifecycleCommand
	if req.LifecycleController != nil {
		commands = req.LifecycleController.commands
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case command := <-commands:
			result, err := e.applyServiceLifecycleCommand(ctx, req, state, baseRT, command)
			command.result <- serviceLifecycleResponse{result: result, err: err}
			watchServiceHandles(state, exits, watched)
		case ex := <-exits:
			e.handleUnexpectedServiceExit(ctx, req, inst, state, ex)
		}
	}
}

// watchServiceHandles installs at most one waiter per task generation. A
// replacement gets a new generation, so the old waiter may finish later
// without being mistaken for the current process by handleUnexpectedServiceExit.
func watchServiceHandles(state *runState, exits chan<- serviceExit, watched map[string]uint64) {
	for task := range state.snapshotServices() {
		snapshot, ok := state.serviceSnapshot(task)
		if !ok || watched[task] == snapshot.generation {
			continue
		}
		watched[task] = snapshot.generation
		go func(task string, current serviceSnapshot) {
			exits <- serviceExit{task: task, generation: current.generation, err: current.handle.Wait()}
		}(task, snapshot)
	}
}

func (e *Engine) handleUnexpectedServiceExit(ctx context.Context, req Request, inst *api.Instance, state *runState, exited serviceExit) {
	current, ok := state.serviceSnapshot(exited.task)
	if !ok || current.generation != exited.generation {
		// A deliberate stop/restart leaves a waiter for the previous handle
		// behind. Its eventual result must not affect the replacement.
		return
	}
	node := state.statusSnapshot()[exited.task]
	previous := node.State
	if !state.removeServiceGeneration(exited.task, exited.generation) {
		return
	}
	if exited.err != nil {
		state.setErrorState(exited.task, ctx, node.LastRunKey, fmt.Errorf("service exited: %w", exited.err), 0)
	} else {
		state.setNodeState(exited.task, api.StateStopped, node.LastRunKey, "", 0)
	}
	updated := state.statusSnapshot()[exited.task]
	e.publish(api.Event{
		TS:            process.NowRFC3339Nano(),
		Type:          api.EventProcessExited,
		InstanceID:    inst.ID,
		Worktree:      req.Worktree,
		Target:        req.Target,
		Task:          exited.task,
		Mode:          req.Mode,
		PID:           node.PID,
		State:         updated.State,
		PreviousState: previous,
		Error:         updated.LastError,
	})
}

func (e *Engine) applyServiceLifecycleCommand(ctx context.Context, req Request, state *runState, baseRT *project.Runtime, command serviceLifecycleCommand) (ServiceLifecycleResult, error) {
	result := ServiceLifecycleResult{Task: command.task, Action: command.action}
	task, ok := e.graph.Tasks[command.task]
	if !ok {
		return result, fmt.Errorf("unknown task %q", command.task)
	}
	if !project.IsServiceKind(task.Kind) {
		return result, fmt.Errorf("task %q is not a service", command.task)
	}
	current, ok := state.serviceSnapshot(command.task)
	if !ok {
		if command.action == "restart" {
			return e.startStoppedService(ctx, state, baseRT, task, result)
		}
		return result, fmt.Errorf("service %q is not running; no %s occurred", command.task, command.action)
	}
	result.Previous = ServiceIdentity{PID: current.handle.PID(), Generation: current.generation}
	wasAlive := current.handle.Alive()
	node := state.statusSnapshot()[command.task]
	if command.action == "restart" {
		state.setNodeState(command.task, api.StateRestarting, node.LastRunKey, "", current.handle.PID())
	}
	stopErr := current.handle.Stop()
	if stopErr == nil && current.handle.Alive() {
		stopErr = fmt.Errorf("service remains alive after stop")
	}
	if err := stopErr; err != nil {
		state.setNodeState(command.task, api.StateDegraded, node.LastRunKey, fmt.Sprintf("failed to stop service for %s: %v", command.action, err), current.handle.PID())
		return result, fmt.Errorf("%s service %q: %w", command.action, command.task, err)
	}
	// A stale registry entry can outlive its process. Record a stop only when
	// this command observed a live handle and confirmed that it became dead.
	result.Stopped = wasAlive && !current.handle.Alive()
	if !state.removeServiceGeneration(command.task, current.generation) {
		return result, fmt.Errorf("service %q changed while %s was in progress", command.task, command.action)
	}
	state.setNodeState(command.task, api.StateStopped, node.LastRunKey, "", 0)
	if command.action == "stop" {
		return result, nil
	}
	if command.action != "restart" {
		return result, fmt.Errorf("unsupported service lifecycle action %q", command.action)
	}

	return e.startStoppedService(ctx, state, baseRT, task, result)
}

func (e *Engine) startStoppedService(ctx context.Context, state *runState, baseRT *project.Runtime, task project.Task, result ServiceLifecycleResult) (ServiceLifecycleResult, error) {
	node := state.statusSnapshot()[task.Name]
	state.setNodeState(task.Name, api.StateStarting, node.LastRunKey, "", 0)
	runtime := baseRT.WithTask(task.Name, instance.LogPath(state.req.Worktree, state.inst.ID, task.Name))
	depKeys := state.depKeySnapshot(task.Deps)
	runtime.DepKeys = append([]string(nil), depKeys...)
	execution := e.executeTask(ctx, state, runtime, task, depKeys)
	if execution.err != nil {
		return result, fmt.Errorf("restart service %q: %w", task.Name, execution.err)
	}
	replacement, ok := state.serviceSnapshot(task.Name)
	if !ok || replacement.generation == result.Previous.Generation {
		return result, fmt.Errorf("restart service %q completed without a replacement process", task.Name)
	}
	result.Current = ServiceIdentity{PID: replacement.handle.PID(), Generation: replacement.generation}
	result.Ready = true
	return result, nil
}

func (s *runState) setNodeState(name string, state api.NodeState, lastRunKey, lastError string, pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	node := s.status[name]
	prev := node.State
	prevPID := node.PID
	prevError := node.LastError
	node.State = state
	if state == api.StateStarting || state == api.StateRestarting || terminalNodeState(state) {
		node.Ready = false
	}
	if lastRunKey != "" {
		node.LastRunKey = lastRunKey
	}
	if lastError == "" {
		node.LastError = ""
		if state != api.StateFailed && state != api.StateDegraded && state != api.StateBlocked {
			node.FailureExcerpts = nil
		}
	} else {
		node.LastError = lastError
	}
	if pid != 0 {
		node.PID = pid
	}
	if state == api.StateStarting || state == api.StateReady || state == api.StateRunning || state == api.StateRestarting {
		if s.nodeStarted[name].IsZero() || terminalNodeState(prev) {
			s.nodeStarted[name] = now
			node.DurationMs = 0
			node.Cache = nil
		}
	}
	if terminalNodeState(state) {
		if started := s.nodeStarted[name]; !started.IsZero() {
			node.DurationMs = durationMilliseconds(now.Sub(started))
		}
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
			DurationMs:    node.DurationMs,
			PID:           node.PID,
			Error:         node.LastError,
		})
	}
}

func (s *runState) setNodeReady(name string, ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.status[name]
	node.Ready = ready
	s.status[name] = node
	s.saveLocked()
}

func terminalNodeState(state api.NodeState) bool {
	switch state {
	case api.StateCached, api.StateDone, api.StateFailed, api.StateMigrationNeeded, api.StateCanceled, api.StateStopped, api.StateSkipped, api.StateBlocked, api.StateDegraded:
		return true
	default:
		return false
	}
}

func durationMilliseconds(duration time.Duration) int64 {
	if duration < 0 {
		return 0
	}
	// Windows and other coarse clocks can report an exact zero tick for completed
	// work. Keep zero/sub-millisecond measurements distinguishable from "not run".
	if ms := duration.Milliseconds(); ms > 0 {
		return ms
	}
	return 1
}

func (s *runState) setErrorState(name string, ctx context.Context, lastRunKey string, err error, pid int) {
	display := truncateDiagnosticText(displayTaskError(ctx, err), 4*1024)
	s.setNodeState(name, classifyTaskError(ctx, err), lastRunKey, display, pid)
	s.mu.Lock()
	node := s.status[name]
	s.mu.Unlock()
	excerpts := boundedFailureExcerpts(node.LogPath, name, s.redactDiagnostic)
	var earlyExit *serviceEarlyExitError
	if len(excerpts) == 0 && errors.As(err, &earlyExit) {
		excerpts = boundedEarlyExitExcerpt(node.LogPath, name, s.redactDiagnostic)
	}
	if len(excerpts) == 0 {
		return
	}
	s.mu.Lock()
	node = s.status[name]
	node.FailureExcerpts = excerpts
	s.status[name] = node
	s.saveLocked()
	s.mu.Unlock()
}

func classifyTaskError(ctx context.Context, err error) api.NodeState {
	if err == nil {
		return api.StateDone
	}
	if errors.Is(err, context.Canceled) || (ctx != nil && errors.Is(ctx.Err(), context.Canceled)) {
		return api.StateCanceled
	}
	if isMigrationNeededError(err) {
		return api.StateMigrationNeeded
	}
	return api.StateFailed
}

type migrationNeededError interface {
	MigrationNeeded() bool
}

func isMigrationNeededError(err error) bool {
	// Error wording is diagnostic text; only an explicit adapter signal may
	// change task state and the actions offered to the operator.
	var migrationNeeded migrationNeededError
	return errors.As(err, &migrationNeeded) && migrationNeeded.MigrationNeeded()
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

func (s *runState) addCacheMiss(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheMisses = append(s.cacheMisses, name)
}

func (s *runState) setCacheTiming(name string, timing api.CacheTiming) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.status[name]
	timingCopy := timing
	node.Cache = &timingCopy
	s.status[name] = node
	s.saveLocked()
}

func (s *runState) recordManifestComputation(task string, computation taskKeyComputation) {
	if len(computation.manifestComponents) == 0 && !computation.localInputsChanged {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manifestUsage == nil {
		return
	}
	if len(computation.manifestComponents) > 0 {
		s.manifestUsage.ReusedTasks = append(s.manifestUsage.ReusedTasks, task)
		s.manifestUsage.ReusedComponents += len(computation.manifestComponents)
	}
	if computation.localInputsChanged {
		s.manifestUsage.LocalInputChangedTasks = append(s.manifestUsage.LocalInputChangedTasks, task)
	}
}

func (s *runState) manifestTiming(computation taskKeyComputation) (int64, []string) {
	if len(computation.manifestComponents) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manifestUsage == nil {
		return 0, nil
	}
	return s.manifestUsage.ValidationDurationMs, append([]string(nil), computation.manifestComponents...)
}

func (s *runState) manifestUsageSnapshot() *api.CacheKeyManifestUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manifestUsage == nil {
		return nil
	}
	usage := *s.manifestUsage
	usage.ReusedTasks = uniqueSortedStrings(usage.ReusedTasks)
	usage.LocalInputChangedTasks = uniqueSortedStrings(usage.LocalInputChangedTasks)
	return &usage
}

func uniqueSortedStrings(values []string) []string {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (s *runState) publishEvent(evt api.Event) {
	if s.publish == nil {
		return
	}
	s.publish(evt)
}

func (s *runState) registerService(task string, handle project.ServiceHandle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serviceGeneration[task]++
	generation := s.serviceGeneration[task]
	s.services[task] = handle
	if pid := handle.PID(); pid > 0 {
		s.inst.Processes[task] = api.ProcessRef{PID: pid, StartedAt: time.Now().UTC(), Generation: generation}
	} else {
		delete(s.inst.Processes, task)
	}
	node := s.status[task]
	node.PID = handle.PID()
	node.Generation = generation
	node.Attempt = int(generation)
	node.Ready = false
	s.status[task] = node
	_ = instance.Save(s.inst)
	s.saveLocked()
}

func (s *runState) serviceHandle(task string) (project.ServiceHandle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	handle, ok := s.services[task]
	return handle, ok
}

func (s *runState) removeServiceGeneration(task string, generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != 0 && s.serviceGeneration[task] != generation {
		return false
	}
	delete(s.services, task)
	delete(s.inst.Processes, task)
	node := s.status[task]
	node.PID = 0
	node.Ready = false
	s.status[task] = node
	_ = instance.Save(s.inst)
	s.saveLocked()
	return true
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

func (s *runState) snapshotCacheMisses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.cacheMisses...)
	sort.Strings(out)
	return out
}

type serviceSnapshot struct {
	handle     project.ServiceHandle
	generation uint64
}

func (s *runState) snapshotServices() map[string]project.ServiceHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]project.ServiceHandle, len(s.services))
	for name, handle := range s.services {
		out[name] = handle
	}
	return out
}

func (s *runState) serviceSnapshot(task string) (serviceSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	handle, ok := s.services[task]
	return serviceSnapshot{handle: handle, generation: s.serviceGeneration[task]}, ok
}

func (s *runState) stopServices(req Request, tasks []string) error {
	var stopErrors []error
	for _, task := range tasks {
		current, ok := s.serviceSnapshot(task)
		if !ok {
			continue
		}
		node := s.statusSnapshot()[task]
		err := current.handle.Stop()
		if err == nil && current.handle.Alive() {
			err = fmt.Errorf("resource remains alive after stop")
		}
		if err != nil {
			stopErr := fmt.Errorf("stop task %q: %w", task, err)
			stopErrors = append(stopErrors, stopErr)
			s.setNodeState(task, api.StateDegraded, node.LastRunKey, stopErr.Error(), current.handle.PID())
			continue
		}
		if !s.removeServiceGeneration(task, current.generation) {
			stopErrors = append(stopErrors, fmt.Errorf("task %q changed during cleanup", task))
			continue
		}
		s.setNodeState(task, api.StateStopped, node.LastRunKey, "", 0)
		s.publishEvent(api.Event{TS: process.NowRFC3339Nano(), Type: api.EventProcessExited, InstanceID: s.inst.ID, Worktree: req.Worktree, Target: req.Target, Task: task, Mode: req.Mode, PID: node.PID, State: api.StateStopped, PreviousState: node.State})
	}
	return errors.Join(stopErrors...)
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
		switch s.status[name].State {
		case api.StateFailed, api.StateMigrationNeeded:
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

func selectedProcessEnv(p project.Project, maps ...map[string]string) map[string]string {
	keys := map[string]bool{}
	for _, values := range maps {
		for key := range values {
			keys[key] = true
		}
	}
	if p != nil {
		for _, task := range p.Tasks() {
			for _, key := range task.Inputs.Env {
				keys[key] = true
			}
			for _, key := range task.RequiredEnv {
				keys[key] = true
			}
		}
		for _, key := range project.RequiredEnvsFor(p) {
			keys[key] = true
		}
	}
	out := map[string]string{}
	for key := range keys {
		if value, ok := os.LookupEnv(key); ok {
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

func sortedHandles(m map[string]project.ServiceHandle) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func normalizeWatchPath(path string) string {
	return filepath.ToSlash(filepath.Clean(path))
}

func watchPathHasPrefix(path, prefix string) bool {
	path = normalizeWatchPath(path)
	prefix = normalizeWatchPath(prefix)
	if prefix == "." || prefix == "" {
		return true
	}
	if path == prefix {
		return true
	}
	return len(path) > len(prefix) && path[:len(prefix)] == prefix && path[len(prefix)] == '/'
}

func splitFlushSyncFiles(worktree, instanceID string, files []string) ([]string, []string) {
	syncDir, err := filepath.Rel(worktree, instance.FlushSyncDir(worktree, instanceID))
	if err != nil {
		return append([]string(nil), files...), nil
	}
	syncDir = normalizeWatchPath(syncDir)
	ids := map[string]bool{}
	userFiles := make([]string, 0, len(files))
	for _, file := range files {
		normalized := normalizeWatchPath(file)
		if !watchPathHasPrefix(normalized, syncDir) {
			userFiles = append(userFiles, file)
			continue
		}
		if filepath.Ext(normalized) != ".sync" {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(normalized), ".sync")
		if id != "" {
			ids[id] = true
		}
	}
	return userFiles, sortedBoolKeys(ids)
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
	candidates := make(map[string]bool, len(downstream))
	for _, name := range downstream {
		if !inClosure[name] {
			continue
		}
		candidates[name] = true
	}
	for _, name := range closure {
		task := e.graph.Tasks[name]
		if project.IsServiceKind(task.Kind) && task.Restart == project.RestartAlways {
			candidates[name] = true
		}
	}
	candidateOrder := sortedBoolKeys(candidates)
	candidateOrder, err = e.graph.TopoSort(candidateOrder)
	if err != nil {
		return nil, filteredDirect
	}
	included := make(map[string]bool, len(candidateOrder))
	filtered := make([]string, 0, len(candidateOrder))
	for _, name := range candidateOrder {
		task := e.graph.Tasks[name]
		if task.Kind == project.KindWarmup && !task.AllowInWatch {
			continue
		}
		if project.IsServiceKind(task.Kind) && task.Restart == project.RestartNever {
			continue
		}
		blockedByDep := false
		for _, dep := range task.Deps {
			if candidates[dep] && !included[dep] {
				blockedByDep = true
				break
			}
		}
		if blockedByDep {
			continue
		}
		included[name] = true
		filtered = append(filtered, name)
	}
	return filtered, filteredDirect
}

func (e *Engine) watchInputPaths(order []string) []string {
	seen := map[string]bool{}
	for _, name := range order {
		task, ok := e.graph.Tasks[name]
		if !ok {
			continue
		}
		for _, path := range task.Inputs.Paths {
			addWatchInputPath(seen, path)
		}
		for _, path := range task.Inputs.Files {
			addWatchInputPath(seen, path)
		}
		for _, path := range task.Inputs.Dirs {
			addWatchInputPath(seen, path)
		}
		for _, pattern := range task.Inputs.Globs {
			addWatchInputPath(seen, globWatchBase(pattern))
		}
		for _, input := range task.Inputs.Filtered {
			if strings.ContainsAny(input.Path, "*?[") {
				addWatchInputPath(seen, globWatchBase(input.Path))
			} else {
				addWatchInputPath(seen, input.Path)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for path := range seen {
		out = append(out, path)
	}
	sort.Strings(out)
	return compactWatchInputPaths(out)
}

func addWatchInputPath(seen map[string]bool, path string) {
	path = filepath.ToSlash(filepath.Clean(path))
	if path == "" {
		return
	}
	if path == "." {
		seen[path] = true
		return
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "../") || path == ".." {
		return
	}
	seen[path] = true
}

func globWatchBase(pattern string) string {
	pattern = filepath.ToSlash(filepath.Clean(pattern))
	if pattern == "." || pattern == "" {
		return "."
	}
	parts := strings.Split(pattern, "/")
	base := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.ContainsAny(part, "*?[") {
			break
		}
		base = append(base, part)
	}
	if len(base) == 0 {
		return "."
	}
	return strings.Join(base, "/")
}

func compactWatchInputPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		covered := false
		for _, existing := range out {
			if existing == "." || path == existing || strings.HasPrefix(path, existing+"/") {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, path)
		}
	}
	return out
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
			if project.IsServiceKind(current.Kind) && project.IsServiceKind(e.graph.Tasks[child].Kind) && !e.graph.Tasks[child].WatchRestartOnServiceDeps {
				continue
			}
			queue = append(queue, child)
		}
	}
	return sortedBoolKeys(seen)
}

func (e *Engine) evaluateFlush(ctx context.Context, req Request, baseRT *project.Runtime, state *runState, flushReq api.FlushRequest) api.FlushResult {
	startedAt := flushReq.CreatedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	status := state.statusSnapshot()
	closure, err := e.graph.TargetClosure(req.Target)
	now := time.Now().UTC()
	result := api.FlushResult{
		RequestID:  flushReq.ID,
		InstanceID: state.inst.ID,
		Worktree:   req.Worktree,
		Project:    e.project.Name(),
		Target:     req.Target,
		Mode:       req.Mode,
		Synced:     true,
		Success:    true,
		DurationMs: now.Sub(startedAt).Milliseconds(),
		UpdatedAt:  now,
	}
	if err != nil {
		result.Success = false
		result.Issues = append(result.Issues, api.FlushIssue{
			Kind:    "target_error",
			Message: err.Error(),
		})
		return result
	}
	for _, name := range closure {
		task := e.graph.Tasks[name]
		node := status[name]
		result.Nodes = append(result.Nodes, node)
		if state.isWatchBlocked(name) {
			result.Success = false
			result.Issues = append(result.Issues, api.FlushIssue{
				Task: name, Kind: "watch_restart_required",
				Message: "inputs changed but watch policy prevents rerunning this task; restart the target explicitly",
				LogPath: node.LogPath,
			})
		}
		if project.IsServiceKind(task.Kind) {
			service := e.evaluateFlushService(ctx, req, baseRT, state, task, node)
			result.Services = append(result.Services, service)
			if !service.Ready {
				result.Success = false
				message := service.Error
				if message == "" {
					message = "service is not healthy"
				}
				result.Issues = append(result.Issues, api.FlushIssue{
					Task:    task.Name,
					Kind:    "service_unhealthy",
					Message: message,
					LogPath: service.LogPath,
				})
			}
			continue
		}
		if node.State == api.StateDone || node.State == api.StateCached {
			continue
		}
		result.Success = false
		result.Issues = append(result.Issues, api.FlushIssue{
			Task:    name,
			Kind:    flushTaskIssueKind(node.State),
			Message: flushTaskIssueMessage(node),
			LogPath: node.LogPath,
		})
	}
	sort.Slice(result.Nodes, func(i, j int) bool { return result.Nodes[i].Name < result.Nodes[j].Name })
	sort.Slice(result.Services, func(i, j int) bool { return result.Services[i].Task < result.Services[j].Task })
	return result
}

func (e *Engine) evaluateFlushService(ctx context.Context, req Request, baseRT *project.Runtime, state *runState, task project.Task, node api.NodeStatus) api.FlushService {
	service := api.FlushService{
		Task:    task.Name,
		State:   node.State,
		PID:     node.PID,
		LogPath: node.LogPath,
	}
	if node.State != api.StateRunning {
		service.Error = fmt.Sprintf("service state is %q, want %q", node.State, api.StateRunning)
		return service
	}
	handle, registered := state.serviceHandle(task.Name)
	if !registered || !handle.Alive() {
		service.Error = "service is not alive"
		return service
	}
	if node.PID > 0 && !instance.ProcessAlive(node.PID) {
		service.Error = "service process is not alive"
		return service
	}
	service.Alive = true
	if task.Ready == nil {
		service.Ready = true
		return service
	}
	timeout := task.ReadyTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rt := baseRT.WithTask(task.Name, instance.LogPath(req.Worktree, state.inst.ID, task.Name))
	ready := make(chan error, 1)
	go func() { ready <- task.Ready(readyCtx, rt) }()
	// Flush can probe the same generation repeatedly. Poll Alive rather than
	// creating another uncancelable handle.Wait goroutine for every probe.
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-ready:
			if !handle.Alive() {
				service.Alive = false
				service.Error = "service is not alive"
			} else if err != nil {
				service.Error = err.Error()
			} else if err := readyCtx.Err(); err != nil {
				service.Error = err.Error()
			} else {
				service.Ready = true
			}
			return service
		case <-readyCtx.Done():
			service.Error = readyCtx.Err().Error()
			return service
		case <-ticker.C:
			if !handle.Alive() {
				service.Alive = false
				service.Error = "service is not alive"
				return service
			}
		}
	}
}

func flushTaskIssueKind(state api.NodeState) string {
	switch state {
	case api.StateFailed:
		return "task_failed"
	case api.StateMigrationNeeded:
		return "migration_needed"
	case api.StateCanceled:
		return "task_canceled"
	default:
		return "task_not_settled"
	}
}

func flushTaskIssueMessage(node api.NodeStatus) string {
	if node.LastError != "" {
		return node.LastError
	}
	return fmt.Sprintf("task state is %q", node.State)
}

func (e *Engine) publish(evt api.Event) {
	e.events.Publish(evt)
}

func finalizeRunResult(result *api.RunResult, state *runState, runErr error) {
	if result == nil || state == nil {
		return
	}
	result.CacheHits = state.snapshotCacheHits()
	result.CacheMisses = state.snapshotCacheMisses()
	result.CacheKeyManifest = state.manifestUsageSnapshot()
	result.Nodes = state.resultNodes()
	if result.FailureExcerpts == nil {
		result.FailureExcerpts = []api.FailureExcerpt{}
	}
	if runErr == nil {
		return
	}
	result.Error = runErr.Error()
	if result.FailedNode == "" {
		result.FailedNode = state.failedNode()
	}
	for _, node := range result.Nodes {
		if node.Name != result.FailedNode {
			continue
		}
		result.FailedNodeLogPath = node.LogPath
		result.LogTail = boundedLogTail(node.LogPath, 50, 32*1024, state.redactDiagnostic)
		if len(node.FailureExcerpts) > 0 {
			result.FailureExcerpts = append([]api.FailureExcerpt(nil), node.FailureExcerpts...)
		} else {
			result.FailureExcerpts = boundedFailureExcerpts(node.LogPath, node.Name, state.redactDiagnostic)
		}
		break
	}
}

func (s *runState) resultNodes() []api.NodeStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := sortedNodeNames(s.status)
	nodes := make([]api.NodeStatus, 0, len(names))
	for _, name := range names {
		node := s.status[name]
		if node.Cache != nil {
			cacheCopy := *node.Cache
			node.Cache = &cacheCopy
		}
		nodes = append(nodes, node)
	}
	return nodes
}

func boundedLogTail(path string, maxLines int, maxBytes int64, sanitizers ...func(string) string) []string {
	if path == "" || maxLines <= 0 || maxBytes <= 0 {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	start := info.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes))
	if err != nil {
		return nil
	}
	if start > 0 {
		if newline := strings.IndexByte(string(data), '\n'); newline >= 0 {
			data = data[newline+1:]
		}
	}
	lines := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	for index := range lines {
		lines[index] = sanitizeDiagnosticLine(lines[index], sanitizers)
	}
	return lines
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
