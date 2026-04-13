package project

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"devflow/pkg/api"
	"devflow/pkg/process"
)

type Kind string

const (
	KindOnce    Kind = "once"
	KindService Kind = "service"
	KindGroup   Kind = "group"
	KindWarmup  Kind = "warmup"
)

type RestartPolicy string

const (
	RestartNever         RestartPolicy = "never"
	RestartOnInputChange RestartPolicy = "on_input_change"
	RestartAlways        RestartPolicy = "always"
)

type FingerprintFunc func(ctx context.Context, rt *Runtime) (string, error)
type CacheKeyFunc func(ctx context.Context, rt *Runtime) (string, error)
type RunFunc func(ctx context.Context, rt *Runtime) error
type ReadyFunc func(ctx context.Context, rt *Runtime) error

type Inputs struct {
	Files  []string
	Dirs   []string
	Env    []string
	Ignore []string
	Custom []FingerprintFunc
}

type Outputs struct {
	Files []string
	Dirs  []string
}

type Task struct {
	Name                      string
	Kind                      Kind
	Deps                      []string
	Inputs                    Inputs
	Outputs                   Outputs
	Run                       RunFunc
	Ready                     ReadyFunc
	ReadyTimeout              time.Duration
	Cache                     bool
	Restart                   RestartPolicy
	WatchRestartOnServiceDeps bool
	AllowInWatch              bool
	Tags                      []string
	Description               string
	Signature                 string
	CacheKeyOverride          CacheKeyFunc
}

type Target struct {
	Name        string
	RootTasks   []string
	Description string
}

type InstanceConfig struct {
	Label     string
	PortNames []string
	Env       map[string]string
	DB        api.DBInstance
	Finalize  func(inst *api.Instance) error
}

type Project interface {
	Name() string
	Tasks() []Task
	Targets() []Target
	ConfigureInstance(ctx context.Context, worktree string) (InstanceConfig, error)
}

type Runtime struct {
	Worktree   string
	Instance   *api.Instance
	Mode       api.RunMode
	Env        map[string]string
	TaskName   string
	LogPath    string
	EventFn    func(api.Event)
	OnService  func(task string, handle *process.Handle)
	onTaskDone func()
	DepKeys    []string
}

func (rt *Runtime) Abs(path string) string {
	return filepath.Join(rt.Worktree, path)
}

func (rt *Runtime) WithTask(taskName, logPath string) *Runtime {
	clone := *rt
	clone.TaskName = taskName
	clone.LogPath = logPath
	return &clone
}

func (rt *Runtime) RunCmd(ctx context.Context, name string, args ...string) error {
	return rt.RunCmdSpec(ctx, process.CommandSpec{
		Name: name,
		Args: args,
		Dir:  rt.Worktree,
		Env:  rt.Env,
	})
}

func (rt *Runtime) RunCmdSpec(ctx context.Context, spec process.CommandSpec) error {
	if spec.Dir == "" {
		spec.Dir = rt.Worktree
	}
	if spec.Env == nil {
		spec.Env = rt.Env
	}
	spec.LogPath = rt.LogPath
	spec.OnLine = func(stream, line string) {
		if rt.EventFn == nil {
			return
		}
		rt.EventFn(api.Event{
			TS:         process.NowRFC3339Nano(),
			Type:       api.EventLogLine,
			InstanceID: rt.Instance.ID,
			Worktree:   rt.Worktree,
			Task:       rt.TaskName,
			Mode:       rt.Mode,
			Stream:     stream,
			Line:       line,
		})
	}

	_, err := process.Run(ctx, spec)
	return err
}

func (rt *Runtime) StartService(ctx context.Context, name string, args ...string) (*process.Handle, error) {
	return rt.StartServiceSpec(ctx, process.CommandSpec{
		Name: name,
		Args: args,
		Dir:  rt.Worktree,
		Env:  rt.Env,
	})
}

func (rt *Runtime) StartServiceSpec(ctx context.Context, spec process.CommandSpec) (*process.Handle, error) {
	if spec.Dir == "" {
		spec.Dir = rt.Worktree
	}
	if spec.Env == nil {
		spec.Env = rt.Env
	}
	spec.LogPath = rt.LogPath
	spec.OnLine = func(stream, line string) {
		if rt.EventFn == nil {
			return
		}
		rt.EventFn(api.Event{
			TS:         process.NowRFC3339Nano(),
			Type:       api.EventLogLine,
			InstanceID: rt.Instance.ID,
			Worktree:   rt.Worktree,
			Task:       rt.TaskName,
			Mode:       rt.Mode,
			Stream:     stream,
			Line:       line,
		})
	}

	handle, err := process.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	if rt.OnService != nil {
		rt.OnService(rt.TaskName, handle)
	}
	return handle, nil
}

func ShellTask(name, description string, kind Kind, deps []string, cache bool, outputs Outputs, inputs Inputs, command string) Task {
	return Task{
		Name:        name,
		Kind:        kind,
		Deps:        deps,
		Cache:       cache,
		Outputs:     outputs,
		Inputs:      inputs,
		Description: description,
		Signature:   command,
		Run: func(ctx context.Context, rt *Runtime) error {
			if kind == KindService {
				_, err := rt.StartService(ctx, "sh", "-c", command)
				return err
			}
			return rt.RunCmd(ctx, "sh", "-c", command)
		},
	}
}

func EnsureCommandExists(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("required command %q not found: %w", name, err)
	}
	return nil
}

func WriteFile(rt *Runtime, rel string, data []byte, mode os.FileMode) error {
	path := rt.Abs(rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}
