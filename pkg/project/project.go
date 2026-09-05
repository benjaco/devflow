package project

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/process"
)

type Kind string

const (
	KindOnce         Kind = "once"
	KindService      Kind = "service"
	KindDebugService Kind = "debug_service"
	KindGroup        Kind = "group"
	KindWarmup       Kind = "warmup"
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
type FileContentFilterFunc func(ctx context.Context, rt *Runtime, file FileContent) ([]byte, error)

type FileContent struct {
	Path    string
	Content []byte
}

type FileContentFilter struct {
	Signature string `json:"signature"`
	fn        FileContentFilterFunc
}

type FilteredInput struct {
	Path   string            `json:"path"`
	Filter FileContentFilter `json:"filter"`
}

type Inputs struct {
	Paths    []string
	Files    []string
	Dirs     []string
	Globs    []string
	Filtered []FilteredInput
	Env      []string
	Ignore   []string
	Custom   []FingerprintFunc
}

type Outputs struct {
	Paths []string
	Files []string
	Dirs  []string
}

type Task struct {
	Name string
	Kind Kind
	// Deps are task graph dependencies that must run before this task.
	Deps []string
	// RequiredCLIs names entries from the project RequiredCLIs catalog.
	// They are external command prerequisites, not task graph edges.
	RequiredCLIs              []string
	RequiredEnv               []string
	Inputs                    Inputs
	Outputs                   Outputs
	BeforeRun                 RunFunc
	Run                       RunFunc
	Ready                     ReadyFunc
	AfterReady                RunFunc
	ReadyTimeout              time.Duration
	Cache                     bool
	Stamp                     bool
	Restart                   RestartPolicy
	WatchRestartOnServiceDeps bool
	AllowInWatch              bool
	Tags                      []string
	Description               string
	Signature                 string
	CacheKeyOverride          CacheKeyFunc
	Debug                     *DebugConfig
}

type DebugConfig struct {
	Type     string
	Host     string
	PortName string
	Protocol string
	Binary   string
	Package  string
}

func IsServiceKind(kind Kind) bool {
	return kind == KindService || kind == KindDebugService
}

type Target struct {
	Name      string
	RootTasks []string
	// RequiredCLIs names external command prerequisites for this target.
	RequiredCLIs []string
	RequiredEnv  []string
	Description  string
}

type InstanceConfig struct {
	Label     string
	PortNames []string
	Env       map[string]string
	DB        api.DBInstance
	Finalize  func(inst *api.Instance) error
}

type PrismaConfig struct {
	SchemaPath    string
	MigrationsDir string
	BasePaths     []string
	CreateOnly    bool
	Command       process.CommandSpec
}

type Project interface {
	Name() string
	Tasks() []Task
	Targets() []Target
	ConfigureInstance(ctx context.Context, worktree string) (InstanceConfig, error)
}

type PrismaConfigProvider interface {
	PrismaConfig() PrismaConfig
}

type CacheNamespacer interface {
	CacheNamespace() string
}

func CacheNamespace(p Project) string {
	if p == nil {
		return "default"
	}
	if namespacer, ok := p.(CacheNamespacer); ok {
		if namespace := namespacer.CacheNamespace(); namespace != "" {
			return namespace
		}
	}
	if name := p.Name(); name != "" {
		return name
	}
	return "default"
}

type Runtime struct {
	Worktree string
	Instance *api.Instance
	Mode     api.RunMode
	Env      map[string]string
	TaskName string
	LogPath  string
	EventFn  func(api.Event)
	// Processes and PID-less resources share one registration path so lifecycle
	// ownership never depends on the resource's concrete implementation.
	OnServiceHandle func(task string, handle ServiceHandle)
	DepKeys         []string
	OnPrompt        func(task string, req process.PromptRequest) (process.PromptResponse, error)
}

// ServiceHandle is the lifecycle boundary for supervised services. Normal
// command services use *process.Handle; integrations such as managed
// containers can implement the same contract without spawning a wrapper CLI.
type ServiceHandle interface {
	PID() int
	Alive() bool
	Wait() error
	Stop() error
}

func (rt *Runtime) Abs(path string) string {
	return filepath.Join(rt.Worktree, path)
}

func (rt *Runtime) CloneEnv() map[string]string {
	return mergeEnvMaps(rt.Env, nil)
}

func (rt *Runtime) EnvWith(overlay map[string]string) map[string]string {
	return mergeEnvMaps(rt.Env, overlay)
}

func (rt *Runtime) LineEmitter() func(string, string) {
	return func(stream, line string) {
		rt.EmitLogLine(stream, line)
	}
}

func (rt *Runtime) EventLineEmitter() func(string, string) {
	return func(stream, line string) {
		rt.emitProcessLine(stream, line)
	}
}

func (rt *Runtime) EmitJSONLine(label string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		rt.EmitLogLine("stderr", label+": "+err.Error())
		return err
	}
	rt.EmitLogLine("stdout", label+": "+string(data))
	return nil
}

func (rt *Runtime) EmitLogLine(stream, line string) {
	if rt == nil {
		return
	}
	if rt.LogPath != "" {
		_ = os.MkdirAll(filepath.Dir(rt.LogPath), 0o755)
		if file, err := os.OpenFile(rt.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			_ = file.Chmod(0o600)
			_, _ = fmt.Fprintf(file, "%s: %s\n", stream, line)
			_ = file.Close()
		}
	}
	if rt.EventFn == nil || rt.Instance == nil {
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
	spec.AppendLog = true
	spec.OnLine = func(stream, line string) {
		rt.emitProcessLine(stream, line)
	}
	if spec.OnPrompt == nil && rt.OnPrompt != nil {
		spec.OnPrompt = func(req process.PromptRequest) (process.PromptResponse, error) {
			return rt.OnPrompt(rt.TaskName, req)
		}
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
	spec.AppendLog = true
	spec.OnLine = func(stream, line string) {
		rt.emitProcessLine(stream, line)
	}
	if spec.OnPrompt == nil && rt.OnPrompt != nil {
		spec.OnPrompt = func(req process.PromptRequest) (process.PromptResponse, error) {
			return rt.OnPrompt(rt.TaskName, req)
		}
	}

	handle, err := process.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	rt.RegisterServiceHandle(handle)
	return handle, nil
}

// RegisterServiceHandle registers a supervised service with the engine.
// StartServiceSpec calls it automatically; managed-resource integrations use
// it directly. The handle must implement idempotent Stop.
func (rt *Runtime) RegisterServiceHandle(handle ServiceHandle) {
	if rt == nil || handle == nil {
		return
	}
	if rt.OnServiceHandle != nil {
		rt.OnServiceHandle(rt.TaskName, handle)
	}
}

func (rt *Runtime) emitProcessLine(stream, line string) {
	if rt == nil || rt.EventFn == nil || rt.Instance == nil {
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
			if IsServiceKind(kind) {
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
