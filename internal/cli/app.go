package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/benjaco/devflow/internal/version"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/cache"
	"github.com/benjaco/devflow/pkg/daemon"
	"github.com/benjaco/devflow/pkg/engine"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
	"github.com/benjaco/devflow/pkg/tui"
)

type App struct {
	Stdout io.Writer
	Stderr io.Writer
}

func New() *App {
	return &App{Stdout: os.Stdout, Stderr: os.Stderr}
}

func (a *App) Run(args []string) error {
	if shouldExecLocalProject(args) {
		return a.execLocalProject(args)
	}
	if len(args) == 0 {
		return a.defaultEntry()
	}
	switch args[0] {
	case "run", "up":
		return a.runCmd(args[1:])
	case "__internal_daemon":
		return a.internalDaemonCmd(args[1:])
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
	case "deps", "clis", "required-clis":
		return a.depsCmd(args[1:])
	case "graph":
		return a.graphCmd(args[1:])
	case "watch":
		return a.watchCmd(args[1:])
	case "flush":
		return a.flushCmd(args[1:])
	case "tui":
		return a.tuiCmd(args[1:])
	case "version":
		return a.versionCmd(args[1:])
	case "upgrade":
		return a.upgradeCmd(args[1:])
	case "docs":
		return a.docsCmd(args[1:])
	default:
		return a.usage()
	}
}

func (a *App) defaultEntry() error {
	root, err := resolveWorktree("")
	if err != nil {
		return err
	}
	plan, err := a.defaultLaunchPlan(root)
	if err != nil {
		return err
	}
	client, _, err := daemon.Ensure(context.Background(), root, plan.projectName)
	if err != nil {
		return err
	}
	if _, err := client.Call(context.Background(), daemon.Request{
		Action:  daemon.ActionWatch,
		Project: plan.projectName,
		Target:  plan.target,
		Detach:  true,
	}); err != nil {
		return err
	}
	waitForInitialStatus(root, plan.instanceID, 3*time.Second)
	return tui.Run(tui.Options{Worktree: root, InstanceID: plan.instanceID})
}

type launchPlan struct {
	projectName   string
	target        string
	instanceID    string
	startDetached bool
}

func (a *App) defaultLaunchPlan(root string) (launchPlan, error) {
	p, err := resolvedProject("", root)
	if err != nil {
		return launchPlan{}, fmt.Errorf("devflow default launch requires a detectable project in %s: %w", root, err)
	}
	target := project.PreferredTarget(p)
	if target == "" {
		return launchPlan{}, fmt.Errorf("project %q does not define a default target", p.Name())
	}
	instanceID, _, err := instance.IDForWorktree(root)
	if err != nil {
		return launchPlan{}, err
	}
	plan := launchPlan{
		projectName: p.Name(),
		target:      target,
		instanceID:  instanceID,
	}
	inst, err := instance.Load(root, instanceID)
	if err != nil {
		if os.IsNotExist(err) {
			plan.startDetached = true
			return plan, nil
		}
		return launchPlan{}, err
	}
	if !instance.ProcessAlive(inst.Supervisor.PID) || inst.LastRun.Mode != api.ModeWatch || inst.LastRun.Target != target {
		plan.startDetached = true
	}
	return plan, nil
}

func (a *App) usage() error {
	_, _ = fmt.Fprintln(a.Stderr, "usage: devflow <run|watch|flush|restart|stop|cache|status|logs|instances|doctor|clis|graph|tui|version|upgrade|docs>")
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
	jsonOut := fs.Bool("json", false, "(--json) emit stable JSON output")
	worktree := fs.String("worktree", "", "project worktree path; defaults to the current directory")
	modeWatch := fs.Bool("watch", false, "(--watch) run in watch mode; for detached automation prefer devflow watch <target> --detach plus devflow flush")
	ciMode := fs.Bool("ci", false, "(--ci) run as a finite CI/readiness probe; service tasks start, pass readiness, then stop before returning")
	detach := fs.Bool("detach", false, "(--detach) launch a detached supervisor and return after it starts; this is not a readiness gate")
	maxParallel := fs.Int("max-parallel", 0, "maximum parallel tasks; 0 uses the engine default")
	projectName := fs.String("project", defaultProject(), "registered project adapter name")
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
	if *ciMode {
		if *detach {
			return fmt.Errorf("run --ci is finite and does not support --detach")
		}
		if *modeWatch {
			return fmt.Errorf("run --ci is finite and does not support --watch")
		}
		return a.runDirect(target, *jsonOut, *worktree, *projectName, api.ModeCI, *maxParallel)
	}
	if *modeWatch {
		return a.watchViaDaemon(target, *jsonOut, *worktree, *projectName, *detach, *maxParallel)
	}
	return a.runViaDaemon(target, *jsonOut, *worktree, *projectName, *detach, *maxParallel)
}

func (a *App) runDirect(target string, jsonOut bool, worktreeFlag, projectName string, mode api.RunMode, maxParallel int) error {
	root, err := resolveWorktree(worktreeFlag)
	if err != nil {
		return err
	}
	p, err := project.Lookup(projectName)
	if err != nil {
		return err
	}
	execProject, resolvedTarget, err := project.ResolveExecutionProject(p, target)
	if err != nil {
		return err
	}
	eng, err := engine.New(execProject, root)
	if err != nil {
		return err
	}
	outcome, runErr := eng.Run(context.Background(), engine.Request{
		Target:      resolvedTarget,
		Worktree:    root,
		Mode:        mode,
		MaxParallel: maxParallel,
	})
	if outcome != nil {
		if jsonOut {
			if err := writeJSON(a.Stdout, outcome.Result); err != nil {
				return err
			}
			return runErr
		}
		_, _ = fmt.Fprintf(a.Stdout, "target=%s instance=%s success=%v cache_hits=%d\n", outcome.Result.Target, outcome.Result.InstanceID, outcome.Result.Success, len(outcome.Result.CacheHits))
	}
	return runErr
}

func (a *App) runViaDaemon(target string, jsonOut bool, worktreeFlag, projectName string, detach bool, maxParallel int) error {
	root, err := resolveWorktree(worktreeFlag)
	if err != nil {
		return err
	}
	client, _, err := daemon.Ensure(context.Background(), root, projectName)
	if err != nil {
		return err
	}
	resp, err := client.Call(context.Background(), daemon.Request{
		Action:      daemon.ActionRun,
		Project:     projectName,
		Target:      target,
		Mode:        api.ModeDev,
		MaxParallel: maxParallel,
		Detach:      detach,
	})
	if detach {
		if resp.Started != nil {
			payload := map[string]any{
				"instanceId": resp.Started.InstanceID,
				"target":     resp.Started.Target,
				"mode":       resp.Started.Mode,
				"detached":   true,
				"pid":        resp.Started.DaemonPID,
				"logPath":    resp.Started.LogPath,
			}
			if jsonOut {
				return writeJSON(a.Stdout, payload)
			}
			_, _ = fmt.Fprintf(a.Stdout, "daemon instance=%s pid=%d target=%s\n", resp.Started.InstanceID, resp.Started.DaemonPID, resp.Started.Target)
		}
		return err
	}
	if resp.Run != nil {
		if jsonOut {
			if writeErr := writeJSON(a.Stdout, resp.Run); writeErr != nil {
				return writeErr
			}
			return err
		}
		_, _ = fmt.Fprintf(a.Stdout, "target=%s instance=%s success=%v cache_hits=%d\n", resp.Run.Target, resp.Run.InstanceID, resp.Run.Success, len(resp.Run.CacheHits))
	}
	return err
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
	return a.watchViaDaemon(target, *jsonOut, *worktree, *projectName, *detach, *maxParallel)
}

func (a *App) watchViaDaemon(target string, jsonOut bool, worktreeFlag, projectName string, detach bool, maxParallel int) error {
	root, err := resolveWorktree(worktreeFlag)
	if err != nil {
		return err
	}
	client, _, err := daemon.Ensure(context.Background(), root, projectName)
	if err != nil {
		return err
	}
	resp, err := client.Call(context.Background(), daemon.Request{
		Action:      daemon.ActionWatch,
		Project:     projectName,
		Target:      target,
		MaxParallel: maxParallel,
	})
	if err != nil {
		return err
	}
	if resp.Started == nil {
		return fmt.Errorf("daemon did not return watch start metadata")
	}
	payload := map[string]any{
		"instanceId": resp.Started.InstanceID,
		"target":     resp.Started.Target,
		"mode":       resp.Started.Mode,
		"detached":   true,
		"pid":        resp.Started.DaemonPID,
		"logPath":    resp.Started.LogPath,
	}
	if detach {
		if jsonOut {
			return writeJSON(a.Stdout, payload)
		}
		_, _ = fmt.Fprintf(a.Stdout, "daemon instance=%s pid=%d target=%s\n", resp.Started.InstanceID, resp.Started.DaemonPID, resp.Started.Target)
		return nil
	}
	if jsonOut {
		if err := writeJSON(a.Stdout, payload); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(a.Stdout, "watching target=%s instance=%s through daemon pid=%d\n", resp.Started.Target, resp.Started.InstanceID, resp.Started.DaemonPID)
	subCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return client.Subscribe(subCtx, func(evt api.Event) {
		if jsonOut {
			_ = writeJSONLine(a.Stdout, evt)
		}
	})
}

func (a *App) flushCmd(args []string) error {
	target := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("flush", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	worktree := fs.String("worktree", "", "")
	instanceID := fs.String("instance", "", "")
	projectName := fs.String("project", "", "")
	timeout := fs.Duration("timeout", 60*time.Second, "")
	maxParallel := fs.Int("max-parallel", 0, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		if target != "" || fs.NArg() != 1 {
			return fmt.Errorf("usage: devflow flush [target]")
		}
		target = fs.Arg(0)
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}

	root, id, err := resolveInstance(*worktree, *instanceID)
	if err != nil {
		return err
	}
	_ = id
	client, _, err := daemon.Ensure(context.Background(), root, *projectName)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+5*time.Second)
	defer cancel()
	resp, callErr := client.Call(ctx, daemon.Request{
		Action:      daemon.ActionFlush,
		Project:     *projectName,
		Target:      target,
		TimeoutMs:   timeout.Milliseconds(),
		MaxParallel: *maxParallel,
	})
	if resp.Flush != nil {
		return a.finishFlush(*resp.Flush, *jsonOut)
	}
	return callErr
}

func (a *App) internalDaemonCmd(args []string) error {
	fs := flag.NewFlagSet("__internal_daemon", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	projectName := fs.String("project", "", "")
	worktree := fs.String("worktree", "", "")
	logPath := fs.String("log-path", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return daemon.Serve(ctx, daemon.Options{
		Worktree: root,
		Project:  *projectName,
		LogPath:  *logPath,
	})
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
	_ = id
	client, _, err := daemon.Ensure(context.Background(), root, *projectName)
	if err != nil {
		return err
	}
	resp, runErr := client.Call(context.Background(), daemon.Request{
		Action:      daemon.ActionRestart,
		Project:     *projectName,
		Task:        task,
		Upstream:    *upstream,
		Downstream:  *downstream,
		MaxParallel: *maxParallel,
	})
	if resp.Run != nil {
		if *jsonOut {
			if err := writeJSON(a.Stdout, resp.Run); err != nil {
				return err
			}
			return runErr
		}
		_, _ = fmt.Fprintf(a.Stdout, "restarted=%s success=%v cache_hits=%d\n", task, resp.Run.Success, len(resp.Run.CacheHits))
	} else if runErr == nil {
		if *jsonOut {
			return writeJSON(a.Stdout, map[string]any{"restarted": task, "success": true})
		}
		_, _ = fmt.Fprintf(a.Stdout, "restarted=%s\n", task)
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
	_ = id
	client, _, err := daemon.Ensure(context.Background(), root, "")
	if err != nil {
		return err
	}
	resp, err := client.Call(context.Background(), daemon.Request{Action: daemon.ActionStop, All: *all, Task: *task})
	if err != nil {
		return err
	}
	stopped := []string{}
	if resp.Stop != nil {
		stopped = resp.Stop.Stopped
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
	projectName := fs.String("project", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return err
	}
	p, err := resolvedProject(*projectName, root)
	if err != nil {
		return err
	}
	store := cache.NewNamespaced(instance.CacheRoot(), project.CacheNamespace(p))
	entries, err := store.List()
	if err != nil {
		return err
	}
	payload := map[string]any{
		"cacheRoot": instance.CacheRoot(),
		"namespace": store.Namespace,
		"project":   p.Name(),
		"entries":   entries,
		"count":     len(entries),
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
	projectName := fs.String("project", "", "")
	task := fs.String("task", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return err
	}
	p, err := resolvedProject(*projectName, root)
	if err != nil {
		return err
	}
	store := cache.NewNamespaced(instance.CacheRoot(), project.CacheNamespace(p))
	if err := store.Invalidate(*task); err != nil {
		return err
	}
	payload := map[string]any{"cacheRoot": instance.CacheRoot(), "namespace": store.Namespace, "project": p.Name(), "task": *task, "invalidated": true}
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
	projectName := fs.String("project", "", "")
	keepPerTask := fs.Int("keep-per-task", 1, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return err
	}
	p, err := resolvedProject(*projectName, root)
	if err != nil {
		return err
	}
	store := cache.NewNamespaced(instance.CacheRoot(), project.CacheNamespace(p))
	removed, err := store.GC(*keepPerTask)
	if err != nil {
		return err
	}
	payload := map[string]any{"cacheRoot": instance.CacheRoot(), "namespace": store.Namespace, "project": p.Name(), "removed": removed, "keepPerTask": *keepPerTask}
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
	_ = id
	client, _, err := daemon.Ensure(context.Background(), root, "")
	if err != nil {
		return err
	}
	resp, err := client.Call(context.Background(), daemon.Request{Action: daemon.ActionStatus})
	if err != nil {
		return err
	}
	if resp.Status == nil {
		return fmt.Errorf("daemon did not return status")
	}
	out := *resp.Status
	if *jsonOut {
		return writeJSON(a.Stdout, out)
	}
	_, _ = fmt.Fprintf(a.Stdout, "instance: %s  target: %s  mode: %s\n", out.InstanceID, out.Target, out.Mode)
	_, _ = fmt.Fprintf(a.Stdout, "worktree: %s\n", out.Worktree)
	if len(out.URLs) > 0 {
		keys := make([]string, 0, len(out.URLs))
		for name := range out.URLs {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, name := range keys {
			parts = append(parts, fmt.Sprintf("%s=%s", name, out.URLs[name]))
		}
		_, _ = fmt.Fprintf(a.Stdout, "urls: %s\n", strings.Join(parts, "  "))
	}
	if out.DB.Name != "" {
		_, _ = fmt.Fprintf(a.Stdout, "db: %s host=%s port=%d container=%s\n", out.DB.Name, out.DB.Host, out.DB.Port, out.DB.ContainerName)
	}
	if out.Supervisor != nil {
		state := "stopped"
		if out.Supervisor.Alive {
			state = "running"
		}
		_, _ = fmt.Fprintf(a.Stdout, "supervisor: %s pid=%d log=%s\n", state, out.Supervisor.PID, out.Supervisor.LogPath)
	}
	_, _ = fmt.Fprintln(a.Stdout)
	for _, node := range out.Nodes {
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
	logPath, err := resolveLogPath(root, id, task)
	if err != nil {
		return err
	}
	lines, err := readLastLines(logPath, *tail)
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
		return followFile(a.Stdout, logPath, *jsonOut, task)
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
	target := fs.String("target", "", "")
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
	cliScope := "project"
	resolvedTarget := ""
	requiredCLIs := project.RequiredCLIsFor(p)
	executionProject := p
	if strings.TrimSpace(*target) != "" {
		cliScope = "target"
		executionProject, resolvedTarget, requiredCLIs, err = requiredCLIsForTargetScope(p, *target)
		if err != nil {
			return err
		}
	}
	eng, err := engine.New(executionProject, root)
	if err != nil {
		return err
	}
	result := api.DoctorResult{
		Worktree:     root,
		InstanceID:   id,
		Project:      p.Name(),
		Target:       resolvedTarget,
		CLIScope:     cliScope,
		ChecksPassed: true,
		Checks: []string{
			"graph: ok",
			"cache_root: " + instance.CacheRoot(),
			"cache_namespace: " + project.CacheNamespace(p),
			"project: " + p.Name(),
			"tasks: " + fmt.Sprintf("%d", len(eng.Graph().Tasks)),
		},
	}
	if resolvedTarget != "" {
		result.Checks = append(result.Checks, "target: "+resolvedTarget)
	}
	if len(requiredCLIs) > 0 {
		statuses := project.CheckRequiredCLIs(requiredCLIs)
		missing := make([]string, 0)
		for _, status := range statuses {
			if status.Installed {
				continue
			}
			result.ChecksPassed = false
			if status.Installable {
				missing = append(missing, status.Name+" (installable)")
			} else {
				missing = append(missing, status.Name)
			}
		}
		if len(missing) == 0 {
			result.Checks = append(result.Checks, fmt.Sprintf("required_clis: ok (%d)", len(statuses)))
		} else {
			result.Warnings = append(result.Warnings, "missing required CLIs: "+strings.Join(missing, ", "))
		}
	} else if cliScope == "target" {
		result.Checks = append(result.Checks, "required_clis: ok (0)")
	}
	if *jsonOut {
		return writeJSON(a.Stdout, result)
	}
	for _, check := range result.Checks {
		_, _ = fmt.Fprintln(a.Stdout, check)
	}
	for _, warning := range result.Warnings {
		_, _ = fmt.Fprintln(a.Stdout, "warning: "+warning)
	}
	return nil
}

func (a *App) depsCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: devflow clis <status|install>")
	}
	switch args[0] {
	case "status":
		return a.depsStatusCmd(args[1:])
	case "install":
		return a.depsInstallCmd(args[1:])
	default:
		return fmt.Errorf("usage: devflow clis <status|install>")
	}
}

func (a *App) depsStatusCmd(args []string) error {
	fs := flag.NewFlagSet("clis status", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	projectName := fs.String("project", defaultProject(), "")
	worktree := fs.String("worktree", "", "")
	target := fs.String("target", "", "")
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
	resolvedTarget, requiredCLIs, err := requiredCLIsForOptionalTargetScope(p, *target)
	if err != nil {
		return err
	}
	statuses := project.CheckRequiredCLIs(requiredCLIs)
	payload := map[string]any{
		"worktree":     root,
		"project":      p.Name(),
		"dependencies": statuses,
		"requiredCLIs": statuses,
	}
	if resolvedTarget != "" {
		payload["target"] = resolvedTarget
		payload["cliScope"] = "target"
	} else {
		payload["cliScope"] = "project"
	}
	if *jsonOut {
		return writeJSON(a.Stdout, payload)
	}
	for _, status := range statuses {
		state := "missing"
		if status.Installed {
			state = "installed"
		}
		installable := ""
		if !status.Installed && status.Installable {
			installable = " installable"
		}
		_, _ = fmt.Fprintf(a.Stdout, "%-16s %-10s %s%s\n", status.Name, state, status.Command, installable)
	}
	return nil
}

func (a *App) depsInstallCmd(args []string) error {
	fs := flag.NewFlagSet("clis install", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	projectName := fs.String("project", defaultProject(), "")
	worktree := fs.String("worktree", "", "")
	target := fs.String("target", "", "")
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
	resolvedTarget, requiredCLIs, err := requiredCLIsForOptionalTargetScope(p, *target)
	if err != nil {
		return err
	}
	result, installErr := project.InstallMissingRequiredCLIs(context.Background(), root, requiredCLIs, func(stream, line string) {
		if *jsonOut {
			return
		}
		_, _ = fmt.Fprintf(a.Stdout, "%s: %s\n", stream, line)
	})
	payload := map[string]any{
		"worktree":       root,
		"project":        p.Name(),
		"installed":      result.Installed,
		"alreadyPresent": result.AlreadyPresent,
		"missingInstall": result.MissingInstall,
		"cliScope":       "project",
	}
	if resolvedTarget != "" {
		payload["target"] = resolvedTarget
		payload["cliScope"] = "target"
	}
	if *jsonOut {
		if err := writeJSON(a.Stdout, payload); err != nil {
			return err
		}
		return installErr
	}
	if len(result.Installed) > 0 {
		_, _ = fmt.Fprintf(a.Stdout, "installed: %s\n", strings.Join(result.Installed, ", "))
	}
	if len(result.AlreadyPresent) > 0 {
		_, _ = fmt.Fprintf(a.Stdout, "already present: %s\n", strings.Join(result.AlreadyPresent, ", "))
	}
	if len(result.MissingInstall) > 0 {
		_, _ = fmt.Fprintf(a.Stdout, "missing install scripts: %s\n", strings.Join(result.MissingInstall, ", "))
	}
	return installErr
}

func requiredCLIsForOptionalTargetScope(p project.Project, target string) (string, []project.RequiredCLI, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", project.RequiredCLIsFor(p), nil
	}
	_, resolvedTarget, requiredCLIs, err := requiredCLIsForTargetScope(p, target)
	return resolvedTarget, requiredCLIs, err
}

func requiredCLIsForTargetScope(p project.Project, target string) (project.Project, string, []project.RequiredCLI, error) {
	executionProject, resolvedTarget, err := project.ResolveExecutionProject(p, strings.TrimSpace(target))
	if err != nil {
		return nil, "", nil, err
	}
	requiredCLIs, err := project.RequiredCLIsForTarget(executionProject, resolvedTarget)
	if err != nil {
		return nil, "", nil, err
	}
	return executionProject, resolvedTarget, requiredCLIs, nil
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

func (a *App) tuiCmd(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	worktree := fs.String("worktree", "", "")
	instanceID := fs.String("instance", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return tui.Run(tui.Options{
		Worktree:   *worktree,
		InstanceID: *instanceID,
	})
}

func (a *App) versionCmd(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: devflow version")
	}
	info := version.Current()
	result := api.VersionResult{
		Version:     info.Version,
		ModulePath:  info.ModulePath,
		GoVersion:   info.GoVersion,
		VCSRevision: info.VCSRevision,
		VCSTime:     info.VCSTime,
		Modified:    info.Modified,
	}
	if *jsonOut {
		return writeJSON(a.Stdout, result)
	}
	_, _ = fmt.Fprintf(a.Stdout, "devflow %s\n", result.Version)
	return nil
}

func (a *App) upgradeCmd(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "")
	versionTarget := fs.String("version", "latest", "")
	direct := fs.Bool("direct", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: devflow upgrade")
	}
	target := strings.TrimSpace(*versionTarget)
	if target == "" {
		target = "latest"
	}
	pkg := version.CommandPackage + "@" + target
	command := []string{"go", "install", pkg}
	started := time.Now()
	cmd := exec.Command(command[0], command[1:]...)
	if *direct {
		cmd.Env = upgradeGoEnv(os.Environ())
	}
	output, err := cmd.CombinedOutput()
	result := api.UpgradeResult{
		Command:       command,
		Package:       version.CommandPackage,
		VersionTarget: target,
		Success:       err == nil,
		DurationMs:    time.Since(started).Milliseconds(),
		Output:        strings.TrimSpace(string(output)),
	}
	if err != nil {
		result.Error = err.Error()
	}
	if *jsonOut {
		if writeErr := writeJSON(a.Stdout, result); writeErr != nil {
			return writeErr
		}
		if err != nil {
			return fmt.Errorf("upgrade failed")
		}
		return nil
	}
	if err != nil {
		if result.Output != "" {
			_, _ = fmt.Fprintln(a.Stdout, result.Output)
		}
		return fmt.Errorf("upgrade failed: %w", err)
	}
	if *direct {
		_, _ = fmt.Fprintf(a.Stdout, "upgraded devflow using GOPROXY=direct %s\n", strings.Join(command, " "))
	} else {
		_, _ = fmt.Fprintf(a.Stdout, "upgraded devflow using %s\n", strings.Join(command, " "))
	}
	if warning := upgradePathWarning(command[0]); warning != "" {
		_, _ = fmt.Fprintf(a.Stdout, "warning: %s\n", warning)
	}
	return nil
}

func upgradeGoEnv(env []string) []string {
	const key = "GOPROXY"
	const value = "GOPROXY=direct"
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, item := range env {
		if strings.HasPrefix(item, key+"=") {
			out = append(out, value)
			replaced = true
			continue
		}
		out = append(out, item)
	}
	if !replaced {
		out = append(out, value)
	}
	return out
}

func upgradePathWarning(goCommand string) string {
	installedPath, err := goInstalledDevflowPath(goCommand)
	if err != nil || installedPath == "" {
		return ""
	}
	pathDevflow, err := exec.LookPath("devflow")
	if err != nil || pathDevflow == "" {
		return ""
	}
	if sameExecutablePath(pathDevflow, installedPath) {
		return ""
	}
	return fmt.Sprintf("go install wrote %s, but your shell resolves devflow to %s; put %s earlier on PATH or replace the shadowing command", installedPath, pathDevflow, filepath.Dir(installedPath))
}

func goInstalledDevflowPath(goCommand string) (string, error) {
	out, err := exec.Command(goCommand, "env", "GOBIN", "GOPATH").Output()
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\r\n"), "\n")
	if len(lines) == 0 {
		return "", fmt.Errorf("go env returned no output")
	}
	binDir := strings.TrimSpace(lines[0])
	if binDir == "" {
		if len(lines) < 2 || strings.TrimSpace(lines[1]) == "" {
			return "", fmt.Errorf("go env returned no GOPATH")
		}
		binDir = filepath.Join(strings.TrimSpace(lines[1]), "bin")
	}
	return filepath.Join(binDir, "devflow"), nil
}

func sameExecutablePath(a, b string) bool {
	aAbs, err := filepath.Abs(a)
	if err == nil {
		a = aAbs
	}
	bAbs, err := filepath.Abs(b)
	if err == nil {
		b = bAbs
	}
	aEval, errA := filepath.EvalSymlinks(a)
	bEval, errB := filepath.EvalSymlinks(b)
	if errA == nil {
		a = aEval
	}
	if errB == nil {
		b = bEval
	}
	if a == b {
		return true
	}
	aInfo, errA := os.Stat(a)
	bInfo, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(aInfo, bInfo)
}

func (a *App) docsCmd(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: devflow docs <setup|development>")
	}
	switch args[0] {
	case "setup", "pipeline":
		return writeUserDocs(a.Stdout, docsBundleSetup)
	case "development", "dev", "daily":
		return writeUserDocs(a.Stdout, docsBundleDevelopment)
	default:
		return fmt.Errorf("usage: devflow docs <setup|development>")
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
	explain := fs.Bool("explain", false, "")
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
	payload := api.GraphAffectedResult{
		Files:            changed,
		DirectlyAffected: direct,
		Downstream:       g.Downstream(direct),
	}
	if *explain {
		impacts := g.ExplainAffectedByFiles(changed)
		payload.Explanations = toAPIGraphImpacts(impacts)
		payload.UnmatchedFiles = graphUnmatchedFiles(changed, impacts)
	}
	if *jsonOut {
		return writeJSON(a.Stdout, payload)
	}
	_, _ = fmt.Fprintf(a.Stdout, "affected: %s\n", strings.Join(direct, ", "))
	if *explain {
		for _, impact := range payload.Explanations {
			state := "affected"
			if !impact.Affected {
				state = "ignored"
			}
			detail := impact.Input
			if impact.Relative != "" {
				detail += " relative=" + impact.Relative
			}
			if impact.Ignore != "" {
				detail += " ignore=" + impact.Ignore
			}
			_, _ = fmt.Fprintf(a.Stdout, "%s: %s -> %s (%s %s)\n", state, impact.File, impact.Task, impact.Reason, detail)
		}
		if len(payload.UnmatchedFiles) > 0 {
			_, _ = fmt.Fprintf(a.Stdout, "unmatched: %s\n", strings.Join(payload.UnmatchedFiles, ", "))
		}
	}
	return nil
}

func toAPIGraphImpacts(impacts []graph.FileImpact) []api.GraphFileImpact {
	out := make([]api.GraphFileImpact, 0, len(impacts))
	for _, impact := range impacts {
		out = append(out, api.GraphFileImpact{
			File:     impact.File,
			Task:     impact.Task,
			Affected: impact.Affected,
			Reason:   impact.Reason,
			Input:    impact.Input,
			Relative: impact.Relative,
			Ignore:   impact.Ignore,
		})
	}
	return out
}

func graphUnmatchedFiles(files []string, impacts []graph.FileImpact) []string {
	matched := map[string]bool{}
	for _, impact := range impacts {
		matched[impact.File] = true
	}
	unmatched := make([]string, 0)
	for _, file := range files {
		cleaned := filepath.ToSlash(filepath.Clean(file))
		if cleaned == "." {
			cleaned = ""
		}
		if !matched[cleaned] {
			unmatched = append(unmatched, cleaned)
		}
	}
	sort.Strings(unmatched)
	return unmatched
}

func defaultProject() string {
	names := project.Names()
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func resolvedProject(name, worktree string) (project.Project, error) {
	if strings.TrimSpace(name) != "" {
		return project.Lookup(name)
	}
	names := project.Names()
	if len(names) == 1 {
		return project.Lookup(names[0])
	}
	if strings.TrimSpace(worktree) != "" {
		return project.Detect(worktree)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no project is registered")
	}
	return nil, fmt.Errorf("multiple projects are registered; pass --project explicitly")
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
		switch node.State {
		case api.StatePending, api.StateReady, api.StateRunning, api.StateDirty:
			node.State = api.StateStopped
			node.PID = 0
			state.Nodes[name] = node
		}
	}
	return instance.SaveStatus(worktree, instanceID, state.Target, state.Mode, state.Nodes)
}

func resolveLogPath(worktree, instanceID, task string) (string, error) {
	if task != "supervisor" {
		return instance.LogPath(worktree, instanceID, task), nil
	}
	inst, err := instance.Load(worktree, instanceID)
	if err != nil {
		return "", err
	}
	if inst.Supervisor.LogPath != "" {
		return inst.Supervisor.LogPath, nil
	}
	return filepath.Join(worktree, ".devflow", "logs", instanceID, "supervisor.log"), nil
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

func waitForPIDExit(pid int, timeout time.Duration) {
	if pid <= 0 || timeout <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitForInitialStatus(worktree, instanceID string, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := instance.LoadStatus(worktree, instanceID); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func newFlushRequestID() string {
	return fmt.Sprintf("flush-%d-%d", time.Now().UTC().UnixNano(), os.Getpid())
}

func newFlushResult(requestID, worktree, instanceID, projectName, target string, startedAt time.Time) api.FlushResult {
	now := time.Now().UTC()
	return api.FlushResult{
		RequestID:  requestID,
		InstanceID: instanceID,
		Worktree:   worktree,
		Project:    projectName,
		Target:     target,
		Mode:       api.ModeWatch,
		Success:    false,
		DurationMs: now.Sub(startedAt).Milliseconds(),
		UpdatedAt:  now,
	}
}

func (a *App) finishFlush(result api.FlushResult, jsonOut bool) error {
	if err := a.writeFlushResult(result, jsonOut); err != nil {
		return err
	}
	if !result.Success {
		return fmt.Errorf("flush failed")
	}
	return nil
}

func (a *App) writeFlushResult(result api.FlushResult, jsonOut bool) error {
	if jsonOut {
		return writeJSON(a.Stdout, result)
	}
	if result.Success {
		_, _ = fmt.Fprintf(a.Stdout, "flush ok target=%s instance=%s synced=%v duration_ms=%d\n", result.Target, result.InstanceID, result.Synced, result.DurationMs)
		return nil
	}
	status := "failed"
	if result.TimedOut {
		status = "timed out"
	}
	_, _ = fmt.Fprintf(a.Stdout, "flush %s target=%s instance=%s synced=%v\n", status, result.Target, result.InstanceID, result.Synced)
	for _, issue := range result.Issues {
		task := ""
		if issue.Task != "" {
			task = " task=" + issue.Task
		}
		logPath := ""
		if issue.LogPath != "" {
			logPath = " log=" + issue.LogPath
		}
		_, _ = fmt.Fprintf(a.Stdout, "%s%s: %s%s\n", issue.Kind, task, issue.Message, logPath)
	}
	return nil
}

func waitForFlushAck(worktree, instanceID, requestID, syncPath string, timeout time.Duration) (api.FlushResult, bool, error) {
	if timeout <= 0 {
		return api.FlushResult{}, false, nil
	}
	deadline := time.Now().Add(timeout)
	nextTouch := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		result, err := instance.LoadFlushAck(worktree, instanceID, requestID)
		if err == nil {
			return result, true, nil
		}
		if !os.IsNotExist(err) {
			return api.FlushResult{}, false, err
		}
		if syncPath != "" && !time.Now().Before(nextTouch) {
			_ = os.WriteFile(syncPath, []byte(requestID+"\n"+time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o644)
			nextTouch = time.Now().Add(250 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return api.FlushResult{}, false, nil
}

func waitForWatchReady(worktree, instanceID string, after time.Time, timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	deadline := time.Now().Add(timeout)
	path := instance.FlushWatchReadyPath(worktree, instanceID)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			readyAt, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
			if parseErr == nil && !readyAt.Before(after) {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func stopAllExtraProcessRefs(worktree, instanceID string, inst *api.Instance) map[string]int {
	refs := map[string]int{}
	if inst != nil && inst.Supervisor.ExecPID <= 0 {
		logPath := inst.Supervisor.LogPath
		if logPath == "" {
			logPath = filepath.Join(worktree, ".devflow", "logs", instanceID, "supervisor.log")
		}
		if pid := supervisorChildPIDFromLog(logPath); pid > 0 {
			refs["executor"] = pid
		}
	}
	if state, err := instance.LoadStatus(worktree, instanceID); err == nil {
		for name, node := range state.Nodes {
			if node.PID > 0 {
				refs[name] = node.PID
			}
		}
	}
	return refs
}

func supervisorChildPIDFromLog(path string) int {
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid := 0
	for _, line := range strings.Split(string(data), "\n") {
		idx := strings.LastIndex(line, "child pid=")
		if idx < 0 {
			continue
		}
		var candidate int
		if _, err := fmt.Sscanf(line[idx:], "child pid=%d", &candidate); err == nil && candidate > 0 {
			pid = candidate
		}
	}
	return pid
}

func supervisorStatus(inst *api.Instance) *api.SupervisorStatus {
	if inst == nil || inst.Supervisor.PID <= 0 {
		return nil
	}
	return &api.SupervisorStatus{
		PID:       inst.Supervisor.PID,
		ExecPID:   inst.Supervisor.ExecPID,
		Alive:     instance.ProcessAlive(inst.Supervisor.PID),
		StartedAt: inst.Supervisor.StartedAt,
		LogPath:   inst.Supervisor.LogPath,
	}
}

func instanceURLs(inst *api.Instance) map[string]string {
	if inst == nil {
		return nil
	}
	urls := map[string]string{}
	if port := inst.Ports["backend"]; port > 0 {
		urls["backend"] = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	if port := inst.Ports["frontend"]; port > 0 {
		urls["frontend"] = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	return urls
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
