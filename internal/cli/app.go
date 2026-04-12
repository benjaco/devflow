package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"devflow/pkg/api"
	"devflow/pkg/cache"
	"devflow/pkg/engine"
	"devflow/pkg/graph"
	"devflow/pkg/instance"
	"devflow/pkg/project"
	"devflow/pkg/tui"
)

type App struct {
	Stdout io.Writer
	Stderr io.Writer
}

func New() *App {
	return &App{Stdout: os.Stdout, Stderr: os.Stderr}
}

func (a *App) Run(args []string) error {
	if len(args) == 0 {
		return a.usage()
	}
	switch args[0] {
	case "run", "up":
		return a.runCmd(args[1:])
	case "__internal_exec":
		return a.internalExecCmd(args[1:])
	case "restart":
		return a.restartCmd(args[1:])
	case "stop":
		return a.stopCmd(args[1:])
	case "cache":
		return a.cacheCmd(args[1:])
	case "status":
		return a.statusCmd(args[1:])
	case "logs":
		return a.logsCmd(args[1:])
	case "instances":
		return a.instancesCmd(args[1:])
	case "doctor":
		return a.doctorCmd(args[1:])
	case "graph":
		return a.graphCmd(args[1:])
	case "watch":
		return a.watchCmd(args[1:])
	case "tui":
		return tui.Run()
	default:
		return a.usage()
	}
}

func (a *App) usage() error {
	_, _ = fmt.Fprintln(a.Stderr, "usage: devflow <run|watch|restart|stop|cache|status|logs|instances|doctor|graph|tui>")
	return flag.ErrHelp
}

func (a *App) runCmd(args []string) error {
	target := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	worktree := fs.String("worktree", "", "")
	modeWatch := fs.Bool("watch", false, "")
	ciMode := fs.Bool("ci", false, "")
	detach := fs.Bool("detach", false, "")
	maxParallel := fs.Int("max-parallel", 0, "")
	projectName := fs.String("project", defaultProject(), "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if target == "" {
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: devflow run <target>")
		}
		target = fs.Arg(0)
	}
	if target == "" {
		return fmt.Errorf("usage: devflow run <target>")
	}
	if *detach {
		mode := api.ModeDev
		if *ciMode {
			mode = api.ModeCI
		} else if *modeWatch {
			mode = api.ModeWatch
		}
		return a.executeDetached(target, *projectName, *worktree, mode, *maxParallel, *jsonOut)
	}
	if *modeWatch {
		return a.executeWatch(target, *jsonOut, *worktree, *projectName, *maxParallel)
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return err
	}
	mode := api.ModeDev
	if *ciMode {
		mode = api.ModeCI
	} else if *modeWatch {
		mode = api.ModeWatch
	}

	p, err := project.Lookup(*projectName)
	if err != nil {
		return err
	}
	eng, err := engine.New(p, root)
	if err != nil {
		return err
	}
	outcome, runErr := eng.Run(context.Background(), engine.Request{
		Target:      target,
		Worktree:    root,
		Mode:        mode,
		MaxParallel: *maxParallel,
	})
	if outcome != nil {
		if *jsonOut {
			return writeJSON(a.Stdout, outcome.Result)
		}
		_, _ = fmt.Fprintf(a.Stdout, "target=%s instance=%s success=%v cache_hits=%d\n", outcome.Result.Target, outcome.Result.InstanceID, outcome.Result.Success, len(outcome.Result.CacheHits))
	}
	return runErr
}

func (a *App) watchCmd(args []string) error {
	target := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	worktree := fs.String("worktree", "", "")
	projectName := fs.String("project", defaultProject(), "")
	detach := fs.Bool("detach", false, "")
	maxParallel := fs.Int("max-parallel", 0, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if target == "" {
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: devflow watch <target>")
		}
		target = fs.Arg(0)
	}
	if target == "" {
		return fmt.Errorf("usage: devflow watch <target>")
	}
	if *detach {
		return a.executeDetached(target, *projectName, *worktree, api.ModeWatch, *maxParallel, *jsonOut)
	}
	return a.executeWatch(target, *jsonOut, *worktree, *projectName, *maxParallel)
}

func (a *App) executeWatch(target string, jsonOut bool, worktreeFlag, projectName string, maxParallel int) error {
	root, err := resolveWorktree(worktreeFlag)
	if err != nil {
		return err
	}
	p, err := project.Lookup(projectName)
	if err != nil {
		return err
	}
	eng, err := engine.New(p, root)
	if err != nil {
		return err
	}
	if jsonOut {
		events := eng.SubscribeEvents()
		go func() {
			for evt := range events {
				_ = writeJSONLine(a.Stdout, evt)
			}
		}()
	}
	return eng.Watch(context.Background(), engine.Request{
		Target:      target,
		Worktree:    root,
		Mode:        api.ModeWatch,
		MaxParallel: maxParallel,
	})
}

func (a *App) internalExecCmd(args []string) error {
	fs := flag.NewFlagSet("__internal_exec", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	target := fs.String("target", "", "")
	projectName := fs.String("project", "", "")
	worktree := fs.String("worktree", "", "")
	mode := fs.String("mode", string(api.ModeDev), "")
	maxParallel := fs.Int("max-parallel", 0, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return err
	}
	p, err := project.Lookup(*projectName)
	if err != nil {
		return err
	}
	eng, err := engine.New(p, root)
	if err != nil {
		return err
	}
	req := engine.Request{
		Target:      *target,
		Worktree:    root,
		Mode:        api.RunMode(*mode),
		MaxParallel: *maxParallel,
	}
	switch req.Mode {
	case api.ModeWatch:
		return eng.Watch(context.Background(), req)
	default:
		_, err := eng.Run(context.Background(), req)
		return err
	}
}

func (a *App) executeDetached(target, projectName, worktreeFlag string, mode api.RunMode, maxParallel int, jsonOut bool) error {
	root, err := resolveWorktree(worktreeFlag)
	if err != nil {
		return err
	}
	inst, err := instance.Resolve(root, filepath.Base(root))
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{
		"__internal_exec",
		"--target", target,
		"--project", projectName,
		"--worktree", root,
		"--mode", string(mode),
	}
	if maxParallel > 0 {
		args = append(args, "--max-parallel", fmt.Sprintf("%d", maxParallel))
	}
	logPath := filepath.Join(root, ".devflow", "logs", inst.ID, "supervisor.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(executable, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := instance.RecordDetachedRun(inst, api.RunConfig{
		Project:     projectName,
		Target:      target,
		Mode:        mode,
		MaxParallel: maxParallel,
		Detached:    true,
	}, cmd.Process.Pid, logPath); err != nil {
		return err
	}
	payload := map[string]any{
		"instanceId": inst.ID,
		"target":     target,
		"mode":       mode,
		"detached":   true,
		"pid":        cmd.Process.Pid,
		"logPath":    logPath,
	}
	if jsonOut {
		return writeJSON(a.Stdout, payload)
	}
	_, _ = fmt.Fprintf(a.Stdout, "detached instance=%s pid=%d target=%s\n", inst.ID, cmd.Process.Pid, target)
	return nil
}

func (a *App) restartCmd(args []string) error {
	task := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		task = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	worktree := fs.String("worktree", "", "")
	instanceID := fs.String("instance", "", "")
	projectName := fs.String("project", defaultProject(), "")
	maxParallel := fs.Int("max-parallel", 0, "")
	upstream := fs.Bool("upstream", false, "")
	downstream := fs.Bool("downstream", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if task == "" {
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: devflow restart <task>")
		}
		task = fs.Arg(0)
	}
	root, id, err := resolveInstance(*worktree, *instanceID)
	if err != nil {
		return err
	}
	p, err := project.Lookup(*projectName)
	if err != nil {
		return err
	}
	g, err := graph.New(p.Tasks(), p.Targets())
	if err != nil {
		return err
	}
	selected, err := restartClosure(g, task, *upstream, *downstream)
	if err != nil {
		return err
	}
	taskDef, ok := g.Tasks[task]
	if !ok {
		return fmt.Errorf("unknown task %q", task)
	}
	if taskDef.Kind == project.KindService {
		inst, err := instance.Load(root, id)
		if err != nil {
			return err
		}
		if !inst.LastRun.Detached || inst.LastRun.Target == "" || inst.LastRun.Project == "" {
			return fmt.Errorf("service restart requires a previously detached run for this instance")
		}
		if err := instance.StopSupervisor(inst); err != nil {
			return err
		}
		return a.executeDetached(inst.LastRun.Target, inst.LastRun.Project, root, inst.LastRun.Mode, inst.LastRun.MaxParallel, *jsonOut)
	}
	targetName := "__restart_" + task
	wrapped := restartProject{base: p, target: project.Target{Name: targetName, RootTasks: selected}}
	eng, err := engine.New(wrapped, root)
	if err != nil {
		return err
	}
	outcome, runErr := eng.Run(context.Background(), engine.Request{
		Target:      targetName,
		Worktree:    root,
		Mode:        api.ModeDev,
		MaxParallel: *maxParallel,
	})
	if outcome != nil {
		if *jsonOut {
			return writeJSON(a.Stdout, outcome.Result)
		}
		_, _ = fmt.Fprintf(a.Stdout, "restarted=%s success=%v cache_hits=%d\n", task, outcome.Result.Success, len(outcome.Result.CacheHits))
	}
	return runErr
}

func (a *App) stopCmd(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	worktree := fs.String("worktree", "", "")
	instanceID := fs.String("instance", "", "")
	task := fs.String("task", "", "")
	all := fs.Bool("all", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*all && *task == "" {
		return fmt.Errorf("usage: devflow stop --task <name> | --all")
	}
	root, id, err := resolveInstance(*worktree, *instanceID)
	if err != nil {
		return err
	}
	inst, err := instance.Load(root, id)
	if err != nil {
		return err
	}
	taskName := ""
	if !*all {
		taskName = *task
	}
	if *all && inst.Supervisor.PID > 0 {
		if err := instance.StopSupervisor(inst); err != nil {
			return err
		}
		if err := markAllStoppedNodes(root, id); err != nil {
			return err
		}
		payload := map[string]any{
			"instanceId": id,
			"stopped":    []string{"supervisor"},
		}
		if *jsonOut {
			return writeJSON(a.Stdout, payload)
		}
		_, _ = fmt.Fprintln(a.Stdout, "stopped detached supervisor")
		return nil
	}
	stopped, err := instance.StopProcesses(inst, taskName)
	if err != nil {
		return err
	}
	if err := markStoppedNodes(root, id, stopped); err != nil {
		return err
	}
	payload := map[string]any{
		"instanceId": id,
		"stopped":    stopped,
	}
	if *jsonOut {
		return writeJSON(a.Stdout, payload)
	}
	_, _ = fmt.Fprintf(a.Stdout, "stopped: %s\n", strings.Join(stopped, ", "))
	return nil
}

func (a *App) cacheCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: devflow cache <status|invalidate|gc>")
	}
	switch args[0] {
	case "status":
		return a.cacheStatusCmd(args[1:])
	case "invalidate":
		return a.cacheInvalidateCmd(args[1:])
	case "gc":
		return a.cacheGCCmd(args[1:])
	default:
		return fmt.Errorf("usage: devflow cache <status|invalidate|gc>")
	}
}

func (a *App) cacheStatusCmd(args []string) error {
	fs := flag.NewFlagSet("cache status", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	worktree := fs.String("worktree", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return err
	}
	store := cache.New(instance.CacheRoot(root))
	entries, err := store.List()
	if err != nil {
		return err
	}
	payload := map[string]any{
		"entries": entries,
		"count":   len(entries),
	}
	if *jsonOut {
		return writeJSON(a.Stdout, payload)
	}
	_, _ = fmt.Fprintf(a.Stdout, "entries=%d\n", len(entries))
	for _, entry := range entries {
		_, _ = fmt.Fprintf(a.Stdout, "%s %s %s\n", entry.Task, entry.Key, entry.CreatedAt)
	}
	return nil
}

func (a *App) cacheInvalidateCmd(args []string) error {
	fs := flag.NewFlagSet("cache invalidate", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	worktree := fs.String("worktree", "", "")
	task := fs.String("task", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return err
	}
	store := cache.New(instance.CacheRoot(root))
	if err := store.Invalidate(*task); err != nil {
		return err
	}
	payload := map[string]any{"task": *task, "invalidated": true}
	if *jsonOut {
		return writeJSON(a.Stdout, payload)
	}
	if *task == "" {
		_, _ = fmt.Fprintln(a.Stdout, "invalidated all cache entries")
	} else {
		_, _ = fmt.Fprintf(a.Stdout, "invalidated cache entries for %s\n", *task)
	}
	return nil
}

func (a *App) cacheGCCmd(args []string) error {
	fs := flag.NewFlagSet("cache gc", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	worktree := fs.String("worktree", "", "")
	keepPerTask := fs.Int("keep-per-task", 1, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return err
	}
	store := cache.New(instance.CacheRoot(root))
	removed, err := store.GC(*keepPerTask)
	if err != nil {
		return err
	}
	payload := map[string]any{"removed": removed, "keepPerTask": *keepPerTask}
	if *jsonOut {
		return writeJSON(a.Stdout, payload)
	}
	_, _ = fmt.Fprintf(a.Stdout, "removed=%d keep_per_task=%d\n", removed, *keepPerTask)
	return nil
}

func (a *App) statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	worktree := fs.String("worktree", "", "")
	instanceID := fs.String("instance", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, id, err := resolveInstance(*worktree, *instanceID)
	if err != nil {
		return err
	}
	state, err := instance.LoadStatus(root, id)
	if err != nil {
		return err
	}
	nodes := make([]api.NodeStatus, 0, len(state.Nodes))
	for _, node := range state.Nodes {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	out := api.StatusResult{
		InstanceID: id,
		Target:     state.Target,
		Nodes:      nodes,
	}
	if *jsonOut {
		return writeJSON(a.Stdout, out)
	}
	for _, node := range nodes {
		_, _ = fmt.Fprintf(a.Stdout, "%-20s %-10s %s\n", node.Name, node.Kind, node.State)
	}
	return nil
}

func (a *App) logsCmd(args []string) error {
	task := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		task = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	worktree := fs.String("worktree", "", "")
	instanceID := fs.String("instance", "", "")
	tail := fs.Int("tail", 50, "")
	follow := fs.Bool("follow", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if task == "" {
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: devflow logs <task>")
		}
		task = fs.Arg(0)
	}
	if task == "" {
		return fmt.Errorf("usage: devflow logs <task>")
	}
	root, id, err := resolveInstance(*worktree, *instanceID)
	if err != nil {
		return err
	}
	lines, err := readLastLines(instance.LogPath(root, id, task), *tail)
	if err != nil {
		return err
	}
	if *jsonOut {
		for _, line := range lines {
			if err := writeJSONLine(a.Stdout, map[string]string{"task": task, "line": line}); err != nil {
				return err
			}
		}
	} else {
		for _, line := range lines {
			_, _ = fmt.Fprintln(a.Stdout, line)
		}
	}
	if *follow {
		return followFile(a.Stdout, instance.LogPath(root, id, task), *jsonOut, task)
	}
	return nil
}

func (a *App) instancesCmd(args []string) error {
	fs := flag.NewFlagSet("instances", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	items, err := instance.List()
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeJSON(a.Stdout, items)
	}
	for _, item := range items {
		_, _ = fmt.Fprintf(a.Stdout, "%s %s %s\n", item.ID, item.Label, item.Worktree)
	}
	return nil
}

func (a *App) doctorCmd(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	worktree := fs.String("worktree", "", "")
	projectName := fs.String("project", defaultProject(), "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return err
	}
	id, _, err := instance.IDForWorktree(root)
	if err != nil {
		return err
	}
	p, err := project.Lookup(*projectName)
	if err != nil {
		return err
	}
	eng, err := engine.New(p, root)
	if err != nil {
		return err
	}
	result := api.DoctorResult{
		Worktree:     root,
		InstanceID:   id,
		ChecksPassed: true,
		Checks: []string{
			"graph: ok",
			"cache_root: " + instance.CacheRoot(root),
			"project: " + p.Name(),
			"tasks: " + fmt.Sprintf("%d", len(eng.Graph().Tasks)),
		},
	}
	if *jsonOut {
		return writeJSON(a.Stdout, result)
	}
	for _, check := range result.Checks {
		_, _ = fmt.Fprintln(a.Stdout, check)
	}
	return nil
}

func (a *App) graphCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: devflow graph <list|show|affected>")
	}
	switch args[0] {
	case "list":
		return a.graphListCmd(args[1:])
	case "show":
		return a.graphShowCmd(args[1:])
	case "affected":
		return a.graphAffectedCmd(args[1:])
	default:
		return fmt.Errorf("usage: devflow graph <list|show|affected>")
	}
}

func (a *App) graphListCmd(args []string) error {
	fs := flag.NewFlagSet("graph list", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	projectName := fs.String("project", defaultProject(), "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	g, err := loadGraph(*projectName)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"tasks":   sortedTaskNames(g),
		"targets": sortedTargetNames(g),
	}
	if *jsonOut {
		return writeJSON(a.Stdout, payload)
	}
	_, _ = fmt.Fprintf(a.Stdout, "tasks: %s\n", strings.Join(payload["tasks"].([]string), ", "))
	_, _ = fmt.Fprintf(a.Stdout, "targets: %s\n", strings.Join(payload["targets"].([]string), ", "))
	return nil
}

func (a *App) graphShowCmd(args []string) error {
	target := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("graph show", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	projectName := fs.String("project", defaultProject(), "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if target == "" {
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: devflow graph show <target>")
		}
		target = fs.Arg(0)
	}
	if target == "" {
		return fmt.Errorf("usage: devflow graph show <target>")
	}
	g, err := loadGraph(*projectName)
	if err != nil {
		return err
	}
	closure, err := g.TargetClosure(target)
	if err != nil {
		return err
	}
	payload := map[string]any{"target": target, "closure": closure}
	if *jsonOut {
		return writeJSON(a.Stdout, payload)
	}
	for _, name := range closure {
		_, _ = fmt.Fprintln(a.Stdout, name)
	}
	return nil
}

func (a *App) graphAffectedCmd(args []string) error {
	fs := flag.NewFlagSet("graph affected", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	projectName := fs.String("project", defaultProject(), "")
	files := fs.String("files", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *files == "" {
		return fmt.Errorf("usage: devflow graph affected --files a,b")
	}
	g, err := loadGraph(*projectName)
	if err != nil {
		return err
	}
	changed := splitCSV(*files)
	direct := g.AffectedByFiles(changed)
	payload := map[string]any{
		"files":            changed,
		"directlyAffected": direct,
		"downstream":       g.Downstream(direct),
	}
	if *jsonOut {
		return writeJSON(a.Stdout, payload)
	}
	_, _ = fmt.Fprintf(a.Stdout, "affected: %s\n", strings.Join(direct, ", "))
	return nil
}

func defaultProject() string {
	names := project.Names()
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

type restartProject struct {
	base   project.Project
	target project.Target
}

func (p restartProject) Name() string          { return p.base.Name() }
func (p restartProject) Tasks() []project.Task { return p.base.Tasks() }
func (p restartProject) Targets() []project.Target {
	targets := append([]project.Target(nil), p.base.Targets()...)
	targets = append(targets, p.target)
	return targets
}
func (p restartProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	return p.base.ConfigureInstance(ctx, worktree)
}

func restartClosure(g *graph.Graph, task string, upstream, downstream bool) ([]string, error) {
	if _, ok := g.Tasks[task]; !ok {
		return nil, fmt.Errorf("unknown task %q", task)
	}
	names := []string{task}
	if upstream && downstream {
		up := g.Upstream([]string{task})
		down := g.Downstream(up)
		return g.TopoSort(down)
	}
	if downstream {
		names = g.Downstream([]string{task})
		return g.TopoSort(names)
	}
	if upstream {
		names = g.Upstream([]string{task})
		return g.TopoSort(names)
	}
	return g.TopoSort(names)
}

func resolveWorktree(flagValue string) (string, error) {
	if flagValue != "" {
		return filepath.Abs(flagValue)
	}
	return os.Getwd()
}

func resolveInstance(worktreeFlag, instanceID string) (string, string, error) {
	if instanceID != "" {
		items, err := instance.List()
		if err != nil {
			return "", "", err
		}
		for _, item := range items {
			if item.ID == instanceID {
				return item.Worktree, item.ID, nil
			}
		}
		return "", "", fmt.Errorf("unknown instance %q", instanceID)
	}
	worktree, err := resolveWorktree(worktreeFlag)
	if err != nil {
		return "", "", err
	}
	id, real, err := instance.IDForWorktree(worktree)
	if err != nil {
		return "", "", err
	}
	return real, id, nil
}

func markStoppedNodes(worktree, instanceID string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	state, err := instance.LoadStatus(worktree, instanceID)
	if err != nil {
		return nil
	}
	for _, name := range names {
		node, ok := state.Nodes[name]
		if !ok {
			continue
		}
		node.State = api.StateStopped
		node.PID = 0
		state.Nodes[name] = node
	}
	return instance.SaveStatus(worktree, instanceID, state.Target, state.Mode, state.Nodes)
}

func markAllStoppedNodes(worktree, instanceID string) error {
	state, err := instance.LoadStatus(worktree, instanceID)
	if err != nil {
		return nil
	}
	for name, node := range state.Nodes {
		if node.Kind == "service" {
			node.State = api.StateStopped
			node.PID = 0
			state.Nodes[name] = node
		}
	}
	return instance.SaveStatus(worktree, instanceID, state.Target, state.Mode, state.Nodes)
}

func writeJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func writeJSONLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func loadGraph(projectName string) (*graph.Graph, error) {
	p, err := project.Lookup(projectName)
	if err != nil {
		return nil, err
	}
	return graph.New(p.Tasks(), p.Targets())
}

func sortedTaskNames(g *graph.Graph) []string {
	names := make([]string, 0, len(g.Tasks))
	for name := range g.Tasks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedTargetNames(g *graph.Graph) []string {
	names := make([]string, 0, len(g.Targets))
	for name := range g.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func readLastLines(path string, limit int) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, nil
}

func followFile(w io.Writer, path string, jsonOut bool, task string) error {
	offset := int64(0)
	for {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.Size() > offset {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			if _, err := file.Seek(offset, io.SeekStart); err != nil {
				_ = file.Close()
				return err
			}
			data, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil {
				return err
			}
			offset = info.Size()
			lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			for _, line := range lines {
				if line == "" {
					continue
				}
				if jsonOut {
					if err := writeJSONLine(w, map[string]string{"task": task, "line": line}); err != nil {
						return err
					}
				} else {
					_, _ = fmt.Fprintln(w, line)
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
}
