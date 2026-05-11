package project

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/benjaco/devflow/pkg/process"
)

const (
	defaultDelveCommand = "dlv"
	defaultDebugHost    = "127.0.0.1"
)

type GoDebugServiceBuilder struct {
	task *TaskBuilder

	pkg        string
	binaryPath string
	buildFlags []string
	buildEnv   map[string]string

	dlvCommand      string
	debugHost       string
	debugPortName   string
	appArgs         []string
	continueOnStart bool
	appReady        ReadyFunc
	stopGrace       time.Duration
}

type GoDebugServiceOptions struct {
	Package                   string
	Binary                    string
	BuildFlags                []string
	BuildEnv                  map[string]string
	DlvCommand                string
	DebugHost                 string
	DebugPortName             string
	Args                      []string
	Env                       map[string]string
	EnvPorts                  map[string]string
	StartStopped              bool
	Ready                     ReadyFunc
	StopGrace                 time.Duration
	Deps                      []string
	Inputs                    Inputs
	RequiredCLIs              []string
	Restart                   RestartPolicy
	WatchRestartOnServiceDeps bool
	AllowInWatch              bool
	Description               string
	Signature                 string
	ReadyTimeout              time.Duration
	Tags                      []string
}

func GoDebugService(name string, opts GoDebugServiceOptions) Task {
	spec := goDebugServiceSpec{
		name:            name,
		pkg:             opts.Package,
		binaryPath:      opts.Binary,
		buildFlags:      append([]string(nil), opts.BuildFlags...),
		buildEnv:        mergeEnvMaps(opts.BuildEnv, nil),
		dlvCommand:      opts.DlvCommand,
		debugHost:       opts.DebugHost,
		debugPortName:   opts.DebugPortName,
		appArgs:         append([]string(nil), opts.Args...),
		env:             mergeEnvMaps(opts.Env, nil),
		envPorts:        mergeEnvMaps(opts.EnvPorts, nil),
		continueOnStart: !opts.StartStopped,
		appReady:        opts.Ready,
		stopGrace:       opts.StopGrace,
	}
	spec.applyDefaults()
	restart := opts.Restart
	if restart == "" {
		restart = RestartOnInputChange
	}
	signature := opts.Signature
	if signature == "" {
		signature = spec.signature()
	}
	task := Task{
		Name:                      name,
		Kind:                      KindDebugService,
		Deps:                      append([]string(nil), opts.Deps...),
		RequiredCLIs:              uniqueStrings(append([]string{"go", spec.dlvCommand}, opts.RequiredCLIs...)),
		Inputs:                    opts.Inputs,
		Restart:                   restart,
		WatchRestartOnServiceDeps: opts.WatchRestartOnServiceDeps,
		AllowInWatch:              opts.AllowInWatch,
		Description:               opts.Description,
		Signature:                 signature,
		Debug:                     spec.debugConfig(),
		Ready:                     spec.readyFunc(),
		ReadyTimeout:              opts.ReadyTimeout,
		Tags:                      append([]string(nil), opts.Tags...),
	}
	task.Run = spec.run
	return task
}

func newGoDebugServiceBuilder(b *Builder, name string) *GoDebugServiceBuilder {
	task := b.newTask(name, KindDebugService)
	task.task.Restart = RestartOnInputChange
	task.task.RequiredCLIs = append(task.task.RequiredCLIs, "go", defaultDelveCommand)
	b.RequiredCLIs("go", defaultDelveCommand)

	debugPortName := sanitizeDebugName(name) + "_debug"
	b.Port(debugPortName)

	g := &GoDebugServiceBuilder{
		task:            task,
		pkg:             ".",
		dlvCommand:      defaultDelveCommand,
		debugHost:       defaultDebugHost,
		debugPortName:   debugPortName,
		continueOnStart: true,
		buildEnv:        map[string]string{},
	}
	g.refresh()
	return g
}

func (g *GoDebugServiceBuilder) Ref() TaskRef {
	if g == nil {
		return TaskRef{}
	}
	return g.task.Ref()
}

func (g *GoDebugServiceBuilder) Name() string {
	if g == nil {
		return ""
	}
	return g.task.Name()
}

func (g *GoDebugServiceBuilder) Package(pkg string) *GoDebugServiceBuilder {
	if strings.TrimSpace(pkg) != "" {
		g.pkg = strings.TrimSpace(pkg)
	}
	return g.refresh()
}

func (g *GoDebugServiceBuilder) BuildOutput(path string) *GoDebugServiceBuilder {
	g.binaryPath = strings.TrimSpace(path)
	return g.refresh()
}

func (g *GoDebugServiceBuilder) BuildFlags(flags ...string) *GoDebugServiceBuilder {
	g.buildFlags = append(g.buildFlags, flags...)
	return g.refresh()
}

func (g *GoDebugServiceBuilder) BuildEnv(key, value string) *GoDebugServiceBuilder {
	if key != "" {
		g.buildEnv[key] = value
	}
	return g.refresh()
}

func (g *GoDebugServiceBuilder) DlvCommand(command string) *GoDebugServiceBuilder {
	command = strings.TrimSpace(command)
	if command == "" {
		return g
	}
	g.dlvCommand = command
	g.task.task.RequiredCLIs = append(g.task.task.RequiredCLIs, command)
	g.task.builder.RequiredCLIs(command)
	return g.refresh()
}

func (g *GoDebugServiceBuilder) DebugPort(name string) *GoDebugServiceBuilder {
	name = strings.TrimSpace(name)
	if name == "" {
		return g
	}
	g.debugPortName = name
	g.task.builder.Port(name)
	return g.refresh()
}

func (g *GoDebugServiceBuilder) ListenHost(host string) *GoDebugServiceBuilder {
	host = strings.TrimSpace(host)
	if host == "" {
		return g
	}
	g.debugHost = host
	return g.refresh()
}

func (g *GoDebugServiceBuilder) ContinueOnStart(value bool) *GoDebugServiceBuilder {
	g.continueOnStart = value
	return g.refresh()
}

func (g *GoDebugServiceBuilder) StartStopped() *GoDebugServiceBuilder {
	return g.ContinueOnStart(false)
}

func (g *GoDebugServiceBuilder) Args(args ...string) *GoDebugServiceBuilder {
	g.appArgs = append(g.appArgs, args...)
	return g.refresh()
}

func (g *GoDebugServiceBuilder) Env(key string, value any) *GoDebugServiceBuilder {
	g.task.Env(key, value)
	return g.refresh()
}

func (g *GoDebugServiceBuilder) Inputs(items ...any) *GoDebugServiceBuilder {
	g.task.Inputs(items...)
	return g
}

func (g *GoDebugServiceBuilder) FilteredInput(path any, filter FileContentFilter) *GoDebugServiceBuilder {
	g.task.FilteredInput(path, filter)
	return g
}

func (g *GoDebugServiceBuilder) Ignore(patterns ...string) *GoDebugServiceBuilder {
	g.task.Ignore(patterns...)
	return g
}

func (g *GoDebugServiceBuilder) InputEnv(keys ...string) *GoDebugServiceBuilder {
	g.task.InputEnv(keys...)
	return g
}

func (g *GoDebugServiceBuilder) DependsOn(refs ...any) *GoDebugServiceBuilder {
	g.task.DependsOn(refs...)
	return g
}

func (g *GoDebugServiceBuilder) RequiredCLIs(names ...string) *GoDebugServiceBuilder {
	g.task.RequiredCLIs(names...)
	return g
}

func (g *GoDebugServiceBuilder) Description(value string) *GoDebugServiceBuilder {
	g.task.Description(value)
	return g
}

func (g *GoDebugServiceBuilder) Tags(tags ...string) *GoDebugServiceBuilder {
	g.task.Tags(tags...)
	return g
}

func (g *GoDebugServiceBuilder) Ready(fn ReadyFunc) *GoDebugServiceBuilder {
	g.appReady = fn
	return g.refresh()
}

func (g *GoDebugServiceBuilder) ReadyHTTP(portName, path string, status int) *GoDebugServiceBuilder {
	g.appReady = ReadyHTTPNamedPort(portName, path, status)
	return g.refresh()
}

func (g *GoDebugServiceBuilder) ReadyTCP(portName string) *GoDebugServiceBuilder {
	g.appReady = ReadyTCPPort(portName)
	return g.refresh()
}

func (g *GoDebugServiceBuilder) ReadyFile(path string) *GoDebugServiceBuilder {
	g.appReady = ReadyFile(path)
	return g.refresh()
}

func (g *GoDebugServiceBuilder) ReadyTimeout(timeout time.Duration) *GoDebugServiceBuilder {
	g.task.ReadyTimeout(timeout)
	return g
}

func (g *GoDebugServiceBuilder) StopGrace(grace time.Duration) *GoDebugServiceBuilder {
	g.stopGrace = grace
	return g.refresh()
}

func (g *GoDebugServiceBuilder) Restart(policy RestartPolicy) *GoDebugServiceBuilder {
	g.task.Restart(policy)
	return g
}

func (g *GoDebugServiceBuilder) RestartNever() *GoDebugServiceBuilder {
	g.task.RestartNever()
	return g
}

func (g *GoDebugServiceBuilder) RestartAlways() *GoDebugServiceBuilder {
	g.task.RestartAlways()
	return g
}

func (g *GoDebugServiceBuilder) RestartOnInputChange() *GoDebugServiceBuilder {
	g.task.RestartOnInputChange()
	return g
}

func (g *GoDebugServiceBuilder) WatchRestartOnServiceDeps() *GoDebugServiceBuilder {
	g.task.WatchRestartOnServiceDeps()
	return g
}

func (g *GoDebugServiceBuilder) AllowInWatch() *GoDebugServiceBuilder {
	g.task.AllowInWatch()
	return g
}

func (g *GoDebugServiceBuilder) refresh() *GoDebugServiceBuilder {
	spec := g.spec()
	g.task.task.Debug = spec.debugConfig()
	g.task.task.Ready = spec.readyFunc()
	g.task.task.Signature = spec.signature()
	g.task.task.Run = func(ctx context.Context, rt *Runtime) error {
		local := runtimeWithBuilderEnv(rt, g.task.env)
		return spec.run(ctx, local)
	}
	return g
}

func (g *GoDebugServiceBuilder) spec() goDebugServiceSpec {
	spec := goDebugServiceSpec{
		name:            g.task.Name(),
		pkg:             g.pkg,
		binaryPath:      g.binaryPath,
		buildFlags:      append([]string(nil), g.buildFlags...),
		buildEnv:        mergeEnvMaps(g.buildEnv, nil),
		dlvCommand:      g.dlvCommand,
		debugHost:       g.debugHost,
		debugPortName:   g.debugPortName,
		appArgs:         append([]string(nil), g.appArgs...),
		continueOnStart: g.continueOnStart,
		appReady:        g.appReady,
		stopGrace:       g.stopGrace,
	}
	spec.applyDefaults()
	return spec
}

func (g *GoDebugServiceBuilder) effectiveBinaryPath() string {
	return goDebugBinaryPath(g.task.Name(), g.binaryPath)
}

type goDebugServiceSpec struct {
	name            string
	pkg             string
	binaryPath      string
	buildFlags      []string
	buildEnv        map[string]string
	dlvCommand      string
	debugHost       string
	debugPortName   string
	appArgs         []string
	env             map[string]string
	envPorts        map[string]string
	continueOnStart bool
	appReady        ReadyFunc
	stopGrace       time.Duration
}

func (s *goDebugServiceSpec) applyDefaults() {
	if strings.TrimSpace(s.pkg) == "" {
		s.pkg = "."
	}
	if strings.TrimSpace(s.dlvCommand) == "" {
		s.dlvCommand = defaultDelveCommand
	}
	if strings.TrimSpace(s.debugHost) == "" {
		s.debugHost = defaultDebugHost
	}
	if strings.TrimSpace(s.debugPortName) == "" {
		s.debugPortName = sanitizeDebugName(s.name) + "_debug"
	}
}

func (s goDebugServiceSpec) readyFunc() ReadyFunc {
	debugReady := ReadyTCPPort(s.debugPortName)
	if s.appReady == nil {
		return debugReady
	}
	return ReadyAll(debugReady, s.appReady)
}

func (s goDebugServiceSpec) debugConfig() *DebugConfig {
	return &DebugConfig{
		Type:     "go",
		Host:     s.debugHost,
		PortName: s.debugPortName,
		Protocol: "dap",
		Binary:   s.effectiveBinaryPath(),
		Package:  s.pkg,
	}
}

func (s goDebugServiceSpec) signature() string {
	parts := []string{
		s.pkg,
		s.effectiveBinaryPath(),
		s.dlvCommand,
		s.debugHost,
		s.debugPortName,
		fmt.Sprint(s.continueOnStart),
	}
	parts = append(parts, s.buildFlags...)
	parts = append(parts, s.appArgs...)
	parts = append(parts, envPairs(s.buildEnv)...)
	parts = append(parts, envPairs(s.env)...)
	parts = append(parts, envPairs(s.envPorts)...)
	return "go-debug:" + strings.Join(parts, "\x00")
}

func (s goDebugServiceSpec) run(ctx context.Context, rt *Runtime) error {
	local := s.runtimeWithEnv(rt)
	binary := s.effectiveBinaryPath()
	binaryAbs := binary
	if !filepath.IsAbs(binaryAbs) {
		binaryAbs = local.Abs(binaryAbs)
	}
	if err := os.MkdirAll(filepath.Dir(binaryAbs), 0o755); err != nil {
		return err
	}

	local.EmitLogLine("stdout", fmt.Sprintf("debug: building %s -> %s", s.pkg, binary))
	buildArgs := []string{"build", "-gcflags=all=-N -l"}
	buildArgs = append(buildArgs, s.buildFlags...)
	buildArgs = append(buildArgs, "-o", binaryAbs, s.pkg)
	if err := local.RunCmdSpec(ctx, process.CommandSpec{
		Name: "go",
		Args: buildArgs,
		Dir:  local.Worktree,
		Env:  mergeEnvMaps(local.Env, s.buildEnv),
	}); err != nil {
		return err
	}

	port := 0
	if local.Instance != nil {
		port = local.Instance.Ports[s.debugPortName]
	}
	if port == 0 {
		return fmt.Errorf("named debug port %q not configured", s.debugPortName)
	}
	listen := fmt.Sprintf("%s:%d", s.debugHost, port)
	args := []string{
		"exec", binaryAbs,
		"--headless",
		"--api-version=2",
		"--listen=" + listen,
		"--accept-multiclient",
	}
	if s.continueOnStart {
		args = append(args, "--continue")
	}
	if len(s.appArgs) > 0 {
		args = append(args, "--")
		args = append(args, s.appArgs...)
	}

	local.EmitLogLine("stdout", fmt.Sprintf("debug: starting Delve on %s", listen))
	_, err := local.StartServiceSpec(ctx, process.CommandSpec{
		Name:  s.dlvCommand,
		Args:  args,
		Dir:   local.Worktree,
		Env:   local.Env,
		Grace: s.stopGrace,
	})
	return err
}

func (s goDebugServiceSpec) runtimeWithEnv(rt *Runtime) *Runtime {
	overlay := mergeEnvMaps(s.env, nil)
	if len(s.envPorts) > 0 && overlay == nil {
		overlay = map[string]string{}
	}
	for key, portName := range s.envPorts {
		if rt == nil || rt.Instance == nil {
			overlay[key] = ""
			continue
		}
		overlay[key] = strconv.Itoa(rt.Instance.Ports[portName])
	}
	if len(overlay) == 0 {
		return rt
	}
	clone := *rt
	clone.Env = mergeEnvMaps(rt.Env, overlay)
	return &clone
}

func (s goDebugServiceSpec) effectiveBinaryPath() string {
	return goDebugBinaryPath(s.name, s.binaryPath)
}

func goDebugBinaryPath(name, configured string) string {
	if configured != "" {
		return configured
	}
	name = sanitizeDebugName(name)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.ToSlash(filepath.Join(".devflow", "debug", name))
}

func sanitizeDebugName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "debug"
	}
	var out strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			out.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			out.WriteRune(r)
		case r >= '0' && r <= '9':
			out.WriteRune(r)
		default:
			out.WriteByte('_')
		}
	}
	value := strings.Trim(out.String(), "_")
	if value == "" {
		return "debug"
	}
	return value
}
