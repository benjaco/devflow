package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
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
	Target   string
	Worktree string
	Mode     api.RunMode
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

	status := map[string]api.NodeStatus{}
	for _, name := range order {
		task := e.graph.Tasks[name]
		status[name] = api.NodeStatus{Name: name, Kind: string(task.Kind), State: api.StatePending, LogPath: instance.LogPath(req.Worktree, inst.ID, name)}
	}
	if err := instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, status); err != nil {
		return nil, err
	}

	depKeys := map[string]string{}
	cacheHits := []string{}
	services := map[string]*process.Handle{}
	baseRT := &project.Runtime{
		Worktree: req.Worktree,
		Instance: inst,
		Mode:     req.Mode,
		Env:      cloneMap(inst.Env),
		EventFn: func(evt api.LogEvent) {
			e.logs.Publish(evt)
		},
		OnService: func(task string, handle *process.Handle) {
			services[task] = handle
			inst.Processes[task] = api.ProcessRef{PID: handle.PID(), StartedAt: time.Now().UTC()}
			node := status[task]
			node.PID = handle.PID()
			node.State = api.StateRunning
			status[task] = node
			_ = instance.Save(inst)
			_ = instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, status)
		},
	}

	result := api.RunResult{
		Target:     req.Target,
		Mode:       req.Mode,
		InstanceID: inst.ID,
		Success:    false,
		StartedAt:  started.Format(time.RFC3339),
	}

	for _, name := range order {
		task := e.graph.Tasks[name]
		node := status[name]
		node.State = api.StateReady
		status[name] = node
		_ = instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, status)

		if task.Kind == project.KindGroup {
			node.State = api.StateDone
			status[name] = node
			continue
		}

		node.State = api.StateRunning
		node.LastError = ""
		status[name] = node
		_ = instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, status)

		rt := baseRT.WithTask(name, instance.LogPath(req.Worktree, inst.ID, name))

		if task.Cache {
			key, keyErr := e.taskKey(ctx, rt, task, depKeys)
			if keyErr != nil {
				return nil, keyErr
			}
			node.LastRunKey = key
			if ok, restoreErr := e.cache.Restore(req.Worktree, task.Name, key); restoreErr == nil && ok {
				node.State = api.StateCached
				status[name] = node
				depKeys[name] = key
				cacheHits = append(cacheHits, task.Name)
				_ = instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, status)
				continue
			}
			if runErr := runTask(ctx, task, rt); runErr != nil {
				node.State = api.StateFailed
				node.LastError = runErr.Error()
				status[name] = node
				_ = instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, status)
				result.FailedNode = name
				result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
				result.DurationMs = time.Since(started).Milliseconds()
				return &Outcome{Result: result, Instance: inst}, runErr
			}
			if _, snapErr := e.cache.Snapshot(req.Worktree, task, key); snapErr != nil {
				return nil, snapErr
			}
			depKeys[name] = key
			node.State = api.StateDone
			status[name] = node
			_ = instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, status)
			continue
		}

		if runErr := runTask(ctx, task, rt); runErr != nil {
			node.State = api.StateFailed
			node.LastError = runErr.Error()
			status[name] = node
			_ = instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, status)
			result.FailedNode = name
			result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			result.DurationMs = time.Since(started).Milliseconds()
			return &Outcome{Result: result, Instance: inst}, runErr
		}
		if task.Kind != project.KindService {
			node.State = api.StateDone
			status[name] = node
			_ = instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, status)
		} else if _, ok := services[name]; !ok {
			return nil, fmt.Errorf("service task %q returned without starting a service", name)
		}
	}

	result.Success = true
	result.CacheHits = cacheHits

	if len(services) > 0 && req.Mode != api.ModeCI {
		waitErr := e.waitForServices(ctx, req, inst, status, services)
		if waitErr != nil && !errors.Is(waitErr, context.Canceled) {
			return nil, waitErr
		}
	}

	result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	result.DurationMs = time.Since(started).Milliseconds()
	if err := instance.SaveStatus(req.Worktree, inst.ID, req.Target, req.Mode, status); err != nil {
		return nil, err
	}
	return &Outcome{Result: result, Instance: inst}, nil
}

func runTask(ctx context.Context, task project.Task, rt *project.Runtime) error {
	if task.Run == nil {
		return nil
	}
	return task.Run(ctx, rt)
}

func (e *Engine) taskKey(ctx context.Context, rt *project.Runtime, task project.Task, depKeys map[string]string) (string, error) {
	deps := make([]string, 0, len(task.Deps))
	for _, dep := range task.Deps {
		if key := depKeys[dep]; key != "" {
			deps = append(deps, key)
		}
	}
	inputHashes, envValues, custom, err := fingerprint.CollectTaskInputs(ctx, rt.Worktree, task, rt)
	if err != nil {
		return "", err
	}
	return fingerprint.TaskKey(fingerprint.TaskKeyInput{
		Task:               task,
		DepKeys:            deps,
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
