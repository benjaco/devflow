package project

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/benjaco/devflow/internal/pathspec"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/process"
)

type ConfigureFunc func(context.Context, *Builder) error

type Builder struct {
	name           string
	defaultTarget  string
	cacheNamespace string

	requiredCLIs    []RequiredCLI
	requiredSeen    map[string]bool
	requiredEnv     []string
	requiredEnvSeen map[string]bool

	env       map[string]string
	dotenv    []string
	portNames []string
	portSeen  map[string]bool
	finalize  []func(*api.Instance) error

	tasks     []*TaskBuilder
	taskMap   map[string]*TaskBuilder
	targets   []Target
	actions   []*ActionBuilder
	actionMap map[string]*ActionBuilder

	prismaConfig *PrismaConfig
}

type TaskBuilder struct {
	builder *Builder
	task    Task

	command process.CommandSpec
	dir     string
	env     map[string]envValue
	noCache bool
}

type TaskRef struct {
	name string
}

type PortRef struct {
	name string
}

type ActionBuilder struct {
	builder *Builder
	action  Action
}

type InputGlob string

func Define(fn ConfigureFunc) Project {
	b := NewBuilder()
	if err := fn(context.Background(), b); err != nil {
		panic(err)
	}
	p, err := b.Project()
	if err != nil {
		panic(err)
	}
	return p
}

func NewBuilder() *Builder {
	return &Builder{
		requiredSeen:    map[string]bool{},
		requiredEnvSeen: map[string]bool{},
		env:             map[string]string{},
		portSeen:        map[string]bool{},
		taskMap:         map[string]*TaskBuilder{},
		actionMap:       map[string]*ActionBuilder{},
	}
}

// RequiredEnv declares project-wide environment inputs. Prefer task-level
// RequiredEnv when only a target subset needs a value.
func (b *Builder) RequiredEnv(keys ...string) *Builder {
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || b.requiredEnvSeen[key] {
			continue
		}
		b.requiredEnv = append(b.requiredEnv, key)
		b.requiredEnvSeen[key] = true
	}
	return b
}

func (b *Builder) Name(name string) *Builder {
	b.name = strings.TrimSpace(name)
	return b
}

func (b *Builder) DefaultTarget(name string) *Builder {
	b.defaultTarget = strings.TrimSpace(name)
	return b
}

func (b *Builder) CacheNamespace(namespace string) *Builder {
	b.cacheNamespace = strings.TrimSpace(namespace)
	return b
}

func (b *Builder) RequiredCLIs(names ...string) *Builder {
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || b.requiredSeen[name] {
			continue
		}
		b.requiredCLIs = append(b.requiredCLIs, RequiredCLI{Name: name, Command: name})
		b.requiredSeen[name] = true
	}
	return b
}

func (b *Builder) RequiredCLI(cli RequiredCLI) *Builder {
	if cli.Name == "" {
		cli.Name = cli.Command
	}
	if cli.Command == "" {
		cli.Command = cli.Name
	}
	if cli.Name == "" || b.requiredSeen[cli.Name] {
		return b
	}
	b.requiredCLIs = append(b.requiredCLIs, cli)
	b.requiredSeen[cli.Name] = true
	return b
}

func (b *Builder) DotEnv(paths ...string) *Builder {
	b.dotenv = append(b.dotenv, paths...)
	return b
}

func (b *Builder) Env(key, value string) *Builder {
	if key != "" {
		b.env[key] = value
	}
	return b
}

func (b *Builder) Port(name string) PortRef {
	name = strings.TrimSpace(name)
	if name == "" {
		return PortRef{}
	}
	if !b.portSeen[name] {
		b.portNames = append(b.portNames, name)
		b.portSeen[name] = true
	}
	return PortRef{name: name}
}

func (b *Builder) Finalize(fn func(*api.Instance) error) *Builder {
	if fn != nil {
		b.finalize = append(b.finalize, fn)
	}
	return b
}

func (b *Builder) PrismaConfig(cfg PrismaConfig) *Builder {
	b.prismaConfig = &cfg
	return b
}

func (b *Builder) Task(name string) *TaskBuilder {
	return b.newTask(name, KindOnce)
}

func (b *Builder) Service(name string) *TaskBuilder {
	task := b.newTask(name, KindService)
	task.task.Restart = RestartOnInputChange
	return task
}

func (b *Builder) GoDebugService(name string) *GoDebugServiceBuilder {
	return newGoDebugServiceBuilder(b, name)
}

func (b *Builder) Group(name string, deps ...any) *TaskBuilder {
	task := b.newTask(name, KindGroup)
	task.DependsOn(deps...)
	return task
}

func (b *Builder) Target(name string, roots ...any) *Builder {
	target := Target{Name: name}
	for _, root := range roots {
		if name := refName(root); name != "" {
			target.RootTasks = append(target.RootTasks, name)
		}
	}
	b.targets = append(b.targets, target)
	return b
}

// VerificationTarget declares a full verification fallback for changes to
// adapter or project configuration that cannot be narrowed to task inputs.
func (b *Builder) VerificationTarget(name string, roots ...any) *Builder {
	b.Target(name, roots...)
	b.targets[len(b.targets)-1].Verification = true
	return b
}

func (b *Builder) Action(id string) *ActionBuilder {
	id = strings.TrimSpace(id)
	if existing := b.actionMap[id]; existing != nil {
		return existing
	}
	action := &ActionBuilder{
		builder: b,
		action:  Action{ID: id},
	}
	b.actions = append(b.actions, action)
	b.actionMap[id] = action
	return action
}

func (b *Builder) Project() (Project, error) {
	if b.name == "" {
		return nil, fmt.Errorf("project name is required")
	}
	if len(b.targets) == 0 {
		return nil, fmt.Errorf("project %q must define at least one target", b.name)
	}
	catalog := map[string]bool{}
	for _, cli := range b.requiredCLIs {
		catalog[cli.Name] = true
	}
	tasks := make([]Task, 0, len(b.tasks))
	for _, task := range b.tasks {
		built := task.build(catalog)
		tasks = append(tasks, built)
	}
	actions := make([]Action, 0, len(b.actions))
	for _, action := range b.actions {
		built := action.build()
		if built.ID != "" {
			actions = append(actions, built)
		}
	}
	return builtProject{
		name:           b.name,
		defaultTarget:  b.defaultTarget,
		cacheNamespace: b.cacheNamespace,
		requiredCLIs:   append([]RequiredCLI(nil), b.requiredCLIs...),
		requiredEnv:    append([]string(nil), b.requiredEnv...),
		dotenv:         append([]string(nil), b.dotenv...),
		env:            cloneStringMap(b.env),
		portNames:      append([]string(nil), b.portNames...),
		finalize:       append([]func(*api.Instance) error(nil), b.finalize...),
		tasks:          tasks,
		targets:        append([]Target(nil), b.targets...),
		actions:        actions,
		prismaConfig:   clonePrismaConfig(b.prismaConfig),
	}, nil
}

func (b *Builder) newTask(name string, kind Kind) *TaskBuilder {
	name = strings.TrimSpace(name)
	if existing := b.taskMap[name]; existing != nil {
		return existing
	}
	task := &TaskBuilder{
		builder: b,
		task: Task{
			Name: name,
			Kind: kind,
		},
		env: map[string]envValue{},
	}
	b.tasks = append(b.tasks, task)
	b.taskMap[name] = task
	return task
}

func (t *TaskBuilder) Ref() TaskRef {
	if t == nil {
		return TaskRef{}
	}
	return TaskRef{name: t.task.Name}
}

func (t *TaskBuilder) Name() string {
	if t == nil {
		return ""
	}
	return t.task.Name
}

func (a *ActionBuilder) Kind(kind string) *ActionBuilder {
	if a != nil {
		a.action.Kind = strings.TrimSpace(kind)
	}
	return a
}

func (a *ActionBuilder) Category(category ActionCategory) *ActionBuilder {
	if a != nil {
		a.action.Category = category
	}
	return a
}

func (a *ActionBuilder) Label(label string) *ActionBuilder {
	if a != nil {
		a.action.Label = strings.TrimSpace(label)
	}
	return a
}

func (a *ActionBuilder) Description(description string) *ActionBuilder {
	if a != nil {
		a.action.Description = strings.TrimSpace(description)
	}
	return a
}

func (a *ActionBuilder) Component(component string) *ActionBuilder {
	if a != nil {
		a.action.Component = strings.TrimSpace(component)
	}
	return a
}

func (a *ActionBuilder) Task(task any) *ActionBuilder {
	if a != nil {
		a.action.Task = refName(task)
	}
	return a
}

func (a *ActionBuilder) Input(input ActionInput) *ActionBuilder {
	if a == nil {
		return a
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return a
	}
	if input.Type == "" {
		input.Type = ActionInputString
	}
	a.action.Inputs = append(a.action.Inputs, input)
	return a
}

func (a *ActionBuilder) Alias(aliases ...string) *ActionBuilder {
	if a != nil {
		a.action.Aliases = append(a.action.Aliases, aliases...)
	}
	return a
}

func (a *ActionBuilder) Writes(paths ...string) *ActionBuilder {
	if a != nil {
		a.action.Effects.Writes = append(a.action.Effects.Writes, paths...)
	}
	return a
}

func (a *ActionBuilder) Touches(resources ...string) *ActionBuilder {
	if a != nil {
		a.action.Effects.Touches = append(a.action.Effects.Touches, resources...)
	}
	return a
}

func (a *ActionBuilder) Invalidates(refs ...any) *ActionBuilder {
	if a == nil {
		return a
	}
	for _, ref := range refs {
		if name := refName(ref); name != "" {
			a.action.Effects.Invalidates = append(a.action.Effects.Invalidates, name)
		}
	}
	return a
}

func (a *ActionBuilder) Relaunch(policy ActionRelaunchPolicy) *ActionBuilder {
	if a != nil {
		a.action.Relaunch = policy
	}
	return a
}

func (a *ActionBuilder) RelaunchPreviousTargetAfterSuccess() *ActionBuilder {
	return a.Relaunch(ActionRelaunchPreviousTargetAfterSuccess)
}

func (a *ActionBuilder) build() Action {
	if a == nil {
		return Action{}
	}
	action := a.action
	action.Aliases = uniqueStrings(action.Aliases)
	action.Inputs = cloneActionInputs(action.Inputs)
	action.Effects = cloneEffects(action.Effects)
	if action.Relaunch == "" {
		action.Relaunch = ActionRelaunchNever
	}
	return action
}

func (t *TaskBuilder) Command(name string, args ...string) *TaskBuilder {
	t.command = process.CommandSpec{Name: name, Args: append([]string(nil), args...)}
	return t
}

func (t *TaskBuilder) CommandSpec(spec process.CommandSpec) *TaskBuilder {
	t.command = spec
	return t
}

func (t *TaskBuilder) Dir(dir string) *TaskBuilder {
	t.dir = dir
	return t
}

func (t *TaskBuilder) Run(fn RunFunc) *TaskBuilder {
	t.task.Run = fn
	return t
}

// BeforeRun registers setup that runs with a task-local Runtime immediately
// before the task starts. The hook may add task-scoped environment values
// without mutating the shared instance environment.
func (t *TaskBuilder) BeforeRun(fn RunFunc) *TaskBuilder {
	t.task.BeforeRun = composeRunFuncs(t.task.BeforeRun, fn)
	return t
}

func (t *TaskBuilder) DependsOn(refs ...any) *TaskBuilder {
	for _, ref := range refs {
		if name := refName(ref); name != "" {
			t.task.Deps = append(t.task.Deps, name)
		}
	}
	return t
}

func (t *TaskBuilder) RequiredCLIs(names ...string) *TaskBuilder {
	t.task.RequiredCLIs = append(t.task.RequiredCLIs, names...)
	return t
}

func (t *TaskBuilder) RequiredEnv(keys ...string) *TaskBuilder {
	t.task.RequiredEnv = append(t.task.RequiredEnv, keys...)
	return t
}

func (t *TaskBuilder) Inputs(items ...any) *TaskBuilder {
	for _, item := range items {
		t.addInput(item)
	}
	return t
}

func (t *TaskBuilder) FilteredInput(path any, filter FileContentFilter) *TaskBuilder {
	t.task.Inputs.Filtered = append(t.task.Inputs.Filtered, Filtered(path, filter))
	return t
}

func (t *TaskBuilder) Ignore(patterns ...string) *TaskBuilder {
	t.task.Inputs.Ignore = append(t.task.Inputs.Ignore, patterns...)
	return t
}

func (t *TaskBuilder) InputEnv(keys ...string) *TaskBuilder {
	t.task.Inputs.Env = append(t.task.Inputs.Env, keys...)
	return t
}

func (t *TaskBuilder) Fingerprint(fn FingerprintFunc) *TaskBuilder {
	if fn != nil {
		t.task.Inputs.Custom = append(t.task.Inputs.Custom, fn)
	}
	return t
}

func (t *TaskBuilder) Outputs(paths ...string) *TaskBuilder {
	t.task.Outputs.Paths = append(t.task.Outputs.Paths, paths...)
	if t.task.Kind == KindOnce && !t.noCache {
		t.task.Cache = true
	}
	return t
}

func (t *TaskBuilder) OutputFiles(paths ...string) *TaskBuilder {
	t.task.Outputs.Files = append(t.task.Outputs.Files, paths...)
	if t.task.Kind == KindOnce && !t.noCache {
		t.task.Cache = true
	}
	return t
}

func (t *TaskBuilder) OutputDirs(paths ...string) *TaskBuilder {
	t.task.Outputs.Dirs = append(t.task.Outputs.Dirs, paths...)
	if t.task.Kind == KindOnce && !t.noCache {
		t.task.Cache = true
	}
	return t
}

func (t *TaskBuilder) NoCache() *TaskBuilder {
	t.noCache = true
	t.task.Cache = false
	return t
}

func (t *TaskBuilder) Stamp() *TaskBuilder {
	t.noCache = true
	t.task.Cache = false
	t.task.Stamp = true
	return t
}

func (t *TaskBuilder) Env(key string, value any) *TaskBuilder {
	if key == "" {
		return t
	}
	t.env[key] = normalizeEnvValue(value)
	return t
}

func (t *TaskBuilder) Description(value string) *TaskBuilder {
	t.task.Description = value
	return t
}

func (t *TaskBuilder) Tags(tags ...string) *TaskBuilder {
	t.task.Tags = append(t.task.Tags, tags...)
	return t
}

func (t *TaskBuilder) Purposes(purposes ...Purpose) *TaskBuilder {
	t.task.Purposes = append(t.task.Purposes, purposes...)
	return t
}

func (t *TaskBuilder) Effects(effects Effects) *TaskBuilder {
	t.task.Effects = cloneTaskEffects(&effects)
	return t
}

func (t *TaskBuilder) Ready(fn ReadyFunc) *TaskBuilder {
	t.task.Ready = fn
	return t
}

// AfterReady registers work that is committed only after a service passes its
// readiness check. An error fails and stops the service startup.
func (t *TaskBuilder) AfterReady(fn RunFunc) *TaskBuilder {
	t.task.AfterReady = composeRunFuncs(t.task.AfterReady, fn)
	return t
}

func (t *TaskBuilder) ReadyHTTP(portName, path string, status int) *TaskBuilder {
	t.task.Ready = ReadyHTTPNamedPort(portName, path, status)
	return t
}

func (t *TaskBuilder) ReadyTCP(portName string) *TaskBuilder {
	t.task.Ready = ReadyTCPPort(portName)
	return t
}

func (t *TaskBuilder) ReadyFile(path string) *TaskBuilder {
	t.task.Ready = ReadyFile(path)
	return t
}

func (t *TaskBuilder) ReadyTimeout(timeout time.Duration) *TaskBuilder {
	t.task.ReadyTimeout = timeout
	return t
}

func (t *TaskBuilder) Restart(policy RestartPolicy) *TaskBuilder {
	t.task.Restart = policy
	return t
}

func (t *TaskBuilder) RestartNever() *TaskBuilder {
	t.task.Restart = RestartNever
	return t
}

func (t *TaskBuilder) RestartAlways() *TaskBuilder {
	t.task.Restart = RestartAlways
	return t
}

func (t *TaskBuilder) RestartOnInputChange() *TaskBuilder {
	t.task.Restart = RestartOnInputChange
	return t
}

func (t *TaskBuilder) AllowInWatch() *TaskBuilder {
	t.task.AllowInWatch = true
	return t
}

func (t *TaskBuilder) WatchRestartOnServiceDeps() *TaskBuilder {
	t.task.WatchRestartOnServiceDeps = true
	return t
}

func (t *TaskBuilder) CacheKey(fn CacheKeyFunc) *TaskBuilder {
	t.task.CacheKeyOverride = fn
	return t
}

func (t *TaskBuilder) build(requiredCatalog map[string]bool) Task {
	task := t.task
	task.Purposes = slices.Clone(task.Purposes)
	task.Effects = cloneTaskEffects(task.Effects)
	task.Deps = uniqueStrings(task.Deps)
	task.RequiredCLIs = uniqueStrings(task.RequiredCLIs)
	task.RequiredEnv = uniqueStrings(task.RequiredEnv)
	if t.command.Name != "" {
		if requiredCatalog[t.command.Name] {
			task.RequiredCLIs = uniqueStrings(append(task.RequiredCLIs, t.command.Name))
		}
		if task.Signature == "" {
			task.Signature = commandSignature(t.command, t.dir, t.env)
		}
		command := t.command
		dir := t.dir
		env := cloneEnvValues(t.env)
		task.Run = func(ctx context.Context, rt *Runtime) error {
			local := runtimeWithBuilderEnv(rt, env)
			spec := command
			if dir != "" {
				spec.Dir = dir
			}
			if spec.Dir != "" && !filepath.IsAbs(spec.Dir) {
				spec.Dir = local.Abs(spec.Dir)
			}
			spec.Env = mergeEnvMaps(local.Env, spec.Env)
			if IsServiceKind(task.Kind) {
				_, err := local.StartServiceSpec(ctx, spec)
				return err
			}
			return local.RunCmdSpec(ctx, spec)
		}
	}
	return task
}

func (t *TaskBuilder) addInput(item any) {
	addInput(&t.task.Inputs, item)
}

// InputsFrom converts the same path, glob, slice, and filtered-input values
// accepted by TaskBuilder.Inputs into a standalone input specification.
func InputsFrom(items ...any) Inputs {
	var inputs Inputs
	for _, item := range items {
		addInput(&inputs, item)
	}
	return inputs
}

func addInput(inputs *Inputs, item any) {
	switch value := item.(type) {
	case nil:
		return
	case string:
		if pathspec.HasGlob(value) {
			inputs.Globs = append(inputs.Globs, value)
		} else {
			inputs.Paths = append(inputs.Paths, value)
		}
	case InputGlob:
		inputs.Globs = append(inputs.Globs, string(value))
	case FilteredInput:
		inputs.Filtered = append(inputs.Filtered, value)
	case []FilteredInput:
		inputs.Filtered = append(inputs.Filtered, value...)
	case []string:
		for _, path := range value {
			addInput(inputs, path)
		}
	default:
		panic(fmt.Sprintf("unsupported input type %T", item))
	}
}

func composeRunFuncs(first, second RunFunc) RunFunc {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return func(ctx context.Context, rt *Runtime) error {
		if err := first(ctx, rt); err != nil {
			return err
		}
		return second(ctx, rt)
	}
}

func Glob(pattern string) InputGlob {
	return InputGlob(pattern)
}

type builtProject struct {
	name           string
	defaultTarget  string
	cacheNamespace string
	requiredCLIs   []RequiredCLI
	requiredEnv    []string
	dotenv         []string
	env            map[string]string
	portNames      []string
	finalize       []func(*api.Instance) error
	tasks          []Task
	targets        []Target
	actions        []Action
	prismaConfig   *PrismaConfig
}

func (p builtProject) Name() string { return p.name }

func (p builtProject) DefaultTarget() string { return p.defaultTarget }

func (p builtProject) CacheNamespace() string {
	if p.cacheNamespace != "" {
		return p.cacheNamespace
	}
	return p.name
}

func (p builtProject) RequiredCLIs() []RequiredCLI {
	return append([]RequiredCLI(nil), p.requiredCLIs...)
}

func (p builtProject) RequiredEnvs() []string {
	return append([]string(nil), p.requiredEnv...)
}

func (p builtProject) Tasks() []Task {
	tasks := slices.Clone(p.tasks)
	for i := range tasks {
		tasks[i].Purposes = slices.Clone(tasks[i].Purposes)
		tasks[i].Effects = cloneTaskEffects(tasks[i].Effects)
	}
	return tasks
}

func (p builtProject) Targets() []Target {
	return append([]Target(nil), p.targets...)
}

func (p builtProject) Actions() []Action {
	actions := make([]Action, 0, len(p.actions))
	for _, action := range p.actions {
		action.Inputs = cloneActionInputs(action.Inputs)
		action.Effects = cloneEffects(action.Effects)
		action.Aliases = append([]string(nil), action.Aliases...)
		actions = append(actions, action)
	}
	return actions
}

func (p builtProject) PrismaConfig() PrismaConfig {
	if p.prismaConfig == nil {
		return PrismaConfig{}
	}
	return *p.prismaConfig
}

func (p builtProject) ConfigureInstance(ctx context.Context, worktree string) (InstanceConfig, error) {
	_ = ctx
	cfg := InstanceConfig{
		Label:     filepath.Base(worktree),
		PortNames: append([]string(nil), p.portNames...),
		Env:       map[string]string{},
	}
	for _, dotenv := range p.dotenv {
		values, err := LoadOptionalDotEnvInWorktree(worktree, dotenv)
		if err != nil {
			return InstanceConfig{}, err
		}
		cfg.Env = mergeEnvMaps(cfg.Env, values)
	}
	cfg.Env = mergeEnvMaps(cfg.Env, p.env)
	finalizers := append([]func(*api.Instance) error(nil), p.finalize...)
	cfg.Finalize = func(inst *api.Instance) error {
		for _, fn := range finalizers {
			if err := fn(inst); err != nil {
				return err
			}
		}
		return nil
	}
	return cfg, nil
}

type envValue interface {
	resolve(*Runtime) string
	signature() string
}

type literalEnvValue string

func (v literalEnvValue) resolve(*Runtime) string { return string(v) }
func (v literalEnvValue) signature() string       { return "literal:" + string(v) }

type portEnvValue string

func (v portEnvValue) resolve(rt *Runtime) string {
	if rt == nil || rt.Instance == nil {
		return ""
	}
	return strconv.Itoa(rt.Instance.Ports[string(v)])
}

func (v portEnvValue) signature() string { return "port:" + string(v) }

func normalizeEnvValue(value any) envValue {
	switch v := value.(type) {
	case PortRef:
		return portEnvValue(v.name)
	case string:
		return literalEnvValue(v)
	case fmt.Stringer:
		return literalEnvValue(v.String())
	default:
		return literalEnvValue(fmt.Sprint(value))
	}
}

func runtimeWithBuilderEnv(rt *Runtime, env map[string]envValue) *Runtime {
	if len(env) == 0 {
		return rt
	}
	overlay := map[string]string{}
	for key, value := range env {
		overlay[key] = value.resolve(rt)
	}
	clone := *rt
	clone.Env = mergeEnvMaps(rt.Env, overlay)
	return &clone
}

func refName(ref any) string {
	switch value := ref.(type) {
	case nil:
		return ""
	case string:
		return value
	case TaskRef:
		return value.name
	case *TaskBuilder:
		return value.Name()
	case *GoDebugServiceBuilder:
		return value.Name()
	default:
		panic(fmt.Sprintf("unsupported task reference type %T", ref))
	}
}

func commandSignature(spec process.CommandSpec, dir string, env map[string]envValue) string {
	payload := struct {
		Name string   `json:"name"`
		Args []string `json:"args"`
		Dir  string   `json:"dir,omitempty"`
		Env  []string `json:"env,omitempty"`
	}{
		Name: spec.Name,
		Args: append([]string(nil), spec.Args...),
		Dir:  firstNonEmpty(dir, spec.Dir),
		Env:  envValuePairs(env),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return spec.Name
	}
	return string(data)
}

func envValuePairs(env map[string]envValue) []string {
	pairs := make([]string, 0, len(env))
	for key, value := range env {
		pairs = append(pairs, key+"="+value.signature())
	}
	sort.Strings(pairs)
	return pairs
}

func cloneEnvValues(in map[string]envValue) map[string]envValue {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]envValue, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func clonePrismaConfig(in *PrismaConfig) *PrismaConfig {
	if in == nil {
		return nil
	}
	out := *in
	out.BasePaths = append([]string(nil), in.BasePaths...)
	return &out
}

func uniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		out = append(out, item)
		seen[item] = true
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
