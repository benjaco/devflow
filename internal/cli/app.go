package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	"github.com/benjaco/devflow/internal/reporepair"
	"github.com/benjaco/devflow/internal/version"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/cache"
	"github.com/benjaco/devflow/pkg/daemon"
	"github.com/benjaco/devflow/pkg/database"
	"github.com/benjaco/devflow/pkg/engine"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
	"github.com/benjaco/devflow/pkg/tui"
	"github.com/benjaco/devflow/pkg/validation"
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
	case "action":
		return a.actionCmd(args[1:])
	case "migration":
		return a.migrationCmd(args[1:])
	case "status":
		return a.statusCmd(args[1:])
	case "logs":
		return a.logsCmd(args[1:])
	case "instances":
		return a.instancesCmd(args[1:])
	case "doctor":
		return a.doctorCmd(args[1:])
	case "clis":
		return a.clisCmd(args[1:])
	case "graph":
		return a.graphCmd(args[1:])
	case "validate":
		return a.validateCmd(args[1:])
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
	return a.launchDefaultTUI(root)
}

func (a *App) launchDefaultTUI(root string) error {
	plan, err := a.defaultLaunchPlan(root)
	if err != nil {
		return err
	}
	client, daemonStarted, err := daemon.Ensure(context.Background(), root, plan.projectName)
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
	waitForInitialStatus(root, plan.instanceID, plan.target, api.ModeWatch, 3*time.Second)
	return tui.Run(tui.Options{Worktree: root, InstanceID: plan.instanceID, StopDaemonOnExit: daemonStarted, Output: a.Stdout})
}

type launchPlan struct {
	projectName string
	target      string
	instanceID  string
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
	// The daemon decides whether existing work can be reused when it admits
	// ActionWatch; persisted status is not evidence of a live engine.
	return launchPlan{
		projectName: p.Name(),
		target:      target,
		instanceID:  instanceID,
	}, nil
}

func (a *App) usage() error {
	_, _ = fmt.Fprintln(a.Stderr, "usage: devflow <run|watch|flush|restart|stop|action|migration|cache|status|logs|instances|doctor|clis|graph|validate|tui|version|upgrade|docs>")
	return flag.ErrHelp
}

func (a *App) validateCmd(args []string) error {
	target := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "emit stable JSON output")
	worktree := fs.String("worktree", "", "project worktree path; defaults to the current directory")
	projectName := fs.String("project", defaultProject(), "registered project adapter name")
	mode := fs.String("mode", string(api.ValidationModeAll), "validation mode: artifacts, orders, or all")
	details := fs.String("details", "", "validation detail level: summary, issues, or full (JSON default: issues)")
	maxListedPaths := fs.Int("max-listed-paths", validation.DefaultMaxListedPaths, "maximum paths listed per validation issue category")
	maxOrders := validation.DefaultMaxOrders
	fs.IntVar(&maxOrders, "max-orders", validation.DefaultMaxOrders, "maximum valid task orders to enumerate exhaustively")
	maxFiles := fs.Int64("max-files", validation.DefaultValidationMaxFiles, "validation-wide maximum files processed")
	maxBytes := fs.Int64("max-bytes", validation.DefaultValidationMaxBytes, "validation-wide maximum logical bytes processed")
	maxTemporaryBytes := fs.Int64("max-temporary-bytes", validation.DefaultValidationMaxTemp, "maximum validation-specific temporary logical bytes")
	diskReserveBytes := fs.Int64("disk-reserve-bytes", validation.DefaultValidationDiskReserve, "free-space safety reserve retained during validation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if target == "" {
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: devflow validate <target> [--mode artifacts|orders|all]")
		}
		target = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return fmt.Errorf("usage: devflow validate <target> [--mode artifacts|orders|all]")
	}
	if maxOrders <= 0 {
		return fmt.Errorf("--max-orders must be positive")
	}
	if *maxListedPaths <= 0 {
		return fmt.Errorf("--max-listed-paths must be positive")
	}
	if *maxFiles <= 0 || *maxBytes <= 0 || *maxTemporaryBytes <= 0 || *diskReserveBytes < 0 {
		return fmt.Errorf("validation resource limits must be positive and --disk-reserve-bytes must not be negative")
	}
	modeValue := api.ValidationMode(strings.ToLower(strings.TrimSpace(*mode)))
	detailsValue := api.ValidationDetails(strings.ToLower(strings.TrimSpace(*details)))
	if detailsValue == "" {
		if *jsonOut {
			detailsValue = api.ValidationDetailsIssues
		} else {
			detailsValue = api.ValidationDetailsSummary
		}
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return err
	}
	p, err := project.Lookup(*projectName)
	if err != nil {
		return err
	}
	execProject, resolvedTarget, err := project.ResolveExecutionProject(p, target)
	if err != nil {
		return err
	}
	validator, err := validation.New(execProject)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := validator.Run(ctx, validation.Request{
		Target:                 resolvedTarget,
		Worktree:               root,
		Mode:                   modeValue,
		Details:                detailsValue,
		MaxOrders:              maxOrders,
		MaxListedPaths:         *maxListedPaths,
		MaxFiles:               *maxFiles,
		MaxBytes:               *maxBytes,
		MaxTemporaryBytes:      *maxTemporaryBytes,
		DiskSafetyReserveBytes: *diskReserveBytes,
		OnEvent: func(evt api.Event) {
			streamValidationProgress(a.Stderr, evt)
		},
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		if err := writeJSON(a.Stdout, result); err != nil {
			return err
		}
	} else {
		artifactTasks := 0
		ordersRun := 0
		if result.Artifacts != nil {
			artifactTasks = len(result.Artifacts.Tasks)
		}
		if result.Orders != nil {
			ordersRun = len(result.Orders.Runs)
		}
		_, _ = fmt.Fprintf(a.Stdout, "validation target=%s mode=%s success=%v artifact_tasks=%d orders=%d\n", result.Target, result.Mode, result.Success, artifactTasks, ordersRun)
		for _, issue := range result.Issues {
			_, _ = fmt.Fprintf(a.Stdout, "%s %s: %s\n", issue.Severity, issue.Kind, issue.Message)
		}
	}
	if !result.Success {
		return validation.ErrValidationFailed
	}
	return nil
}

func streamValidationProgress(out io.Writer, evt api.Event) {
	if out == nil || evt.Type != api.EventValidation {
		return
	}
	state := "update"
	if evt.Done {
		state = "completed"
	} else if evt.DurationMs == 0 {
		state = "started"
	}
	_, _ = fmt.Fprintf(
		out,
		"[devflow] validation phase=%s state=%s files=%d bytes=%d temp_bytes=%d peak_temp_bytes=%d remaining_temp_bytes=%d issues=%d elapsed_ms=%d\n",
		validationPhaseLabel(evt.Phase),
		state,
		evt.FilesProcessed,
		evt.LogicalBytes,
		evt.TemporaryBytes,
		evt.PeakTemporaryBytes,
		evt.RemainingBytes,
		evt.IssueCount,
		evt.DurationMs,
	)
}

func validationPhaseLabel(phase string) string {
	switch phase {
	case "preparing":
		return "Preparing validation"
	case "projecting-inputs":
		return "Projecting inputs"
	case "copying-files":
		return "Copying files"
	case "running-target":
		return "Running the target"
	case "capturing-writes":
		return "Capturing writes"
	case "analyzing-declarations":
		return "Analyzing declarations"
	case "creating-archive":
		return "Creating snapshot/archive"
	case "cleaning-up":
		return "Cleaning up"
	default:
		return phase
	}
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
	detach := fs.Bool("detach", false, "(--detach) launch a detached daemon and return after it starts; this is not a readiness gate")
	maxParallel := fs.Int("max-parallel", 0, "maximum parallel tasks; 0 uses the engine default")
	cacheKeyManifest := fs.String("cache-key-manifest", "", "owner-only cache-key manifest created by devflow cache key --manifest-out")
	commitChanges := fs.Bool("commit-changes", false, "after a successful finite CI run, atomically commit changes allowed by --commit-path")
	var commitPaths repeatedStringFlags
	fs.Var(&commitPaths, "commit-path", "Git pathspec permitted in the repair commit; repeat for additional pathspecs")
	commitMessage := fs.String("commit-message", "", "commit message for --commit-changes")
	pushChanges := fs.Bool("push", false, "push a repository repair commit after creating it")
	failAfterCommit := fs.Bool("fail-after-commit", false, "(--fail-after-commit) return nonzero after a repository repair commit (and requested push) succeeds")
	pedantic := fs.Bool("pedantic", false, "(--pedantic) treat CRLF/LF-only repository changes as commit-worthy in --commit-changes mode")
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
	repairFlagsUsed := len(commitPaths) > 0 || *commitMessage != "" || *pushChanges || *failAfterCommit || *pedantic
	if !*commitChanges && repairFlagsUsed {
		return fmt.Errorf("--commit-path, --commit-message, --push, --fail-after-commit, and --pedantic require --commit-changes")
	}
	var repairOptions *reporepair.Options
	if *commitChanges {
		if !*ciMode {
			return fmt.Errorf("--commit-changes is supported only with run --ci")
		}
		if len(commitPaths) == 0 {
			return fmt.Errorf("--commit-changes requires at least one --commit-path")
		}
		for _, pathspec := range commitPaths {
			if pathspec == "" {
				return fmt.Errorf("--commit-path must not be empty")
			}
			if strings.IndexByte(pathspec, 0) >= 0 {
				return fmt.Errorf("--commit-path must not contain NUL")
			}
		}
		if strings.TrimSpace(*commitMessage) == "" {
			return fmt.Errorf("--commit-changes requires a non-empty --commit-message")
		}
		if strings.IndexByte(*commitMessage, 0) >= 0 {
			return fmt.Errorf("--commit-message must not contain NUL")
		}
		repairOptions = &reporepair.Options{
			Pathspecs:       append([]string(nil), commitPaths...),
			Message:         *commitMessage,
			Push:            *pushChanges,
			FailAfterCommit: *failAfterCommit,
			Pedantic:        *pedantic,
		}
	}
	if *ciMode {
		if *detach {
			return fmt.Errorf("run --ci is finite and does not support --detach")
		}
		if *modeWatch {
			return fmt.Errorf("run --ci is finite and does not support --watch")
		}
		return a.runDirect(target, *jsonOut, *worktree, *projectName, api.ModeCI, *maxParallel, *cacheKeyManifest, repairOptions)
	}
	if *cacheKeyManifest != "" {
		return fmt.Errorf("--cache-key-manifest is supported only with run --ci")
	}
	if *modeWatch {
		return a.watchViaDaemon(target, *jsonOut, *worktree, *projectName, *detach, *maxParallel)
	}
	return a.runViaDaemon(target, *jsonOut, *worktree, *projectName, *detach, *maxParallel)
}

func (a *App) runDirect(target string, jsonOut bool, worktreeFlag, projectName string, mode api.RunMode, maxParallel int, cacheKeyManifest string, repairOptions *reporepair.Options) error {
	commandStarted := time.Now().UTC()
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
	var repairRunner *reporepair.Runner
	if repairOptions != nil {
		repairRunner = reporepair.New(root, *repairOptions, a.Stderr)
	}

	eng, err := engine.New(execProject, root)
	if err != nil {
		result := newDirectRunFailureResult(root, resolvedTarget, mode, commandStarted, err)
		if repairRunner != nil {
			repositoryResult := repairRunner.SkippedDAGFailure()
			result.RepositoryChanges = &repositoryResult
		}
		if jsonOut {
			if writeErr := writeJSON(a.Stdout, result); writeErr != nil {
				return writeErr
			}
		}
		return err
	}
	lease, err := executionstate.Acquire(root, execution.Owner{Target: resolvedTarget, Mode: string(mode), Kind: "ci"})
	if err != nil {
		if jsonOut {
			if writeErr := writeJSON(a.Stdout, newDirectRunFailureResult(root, resolvedTarget, mode, commandStarted, err)); writeErr != nil {
				return writeErr
			}
		}
		return err
	}
	defer lease.Release()
	runCtx := execution.ContextWithLease(context.Background(), lease)
	if repairRunner != nil {
		repositoryResult, preflightErr := repairRunner.Preflight(runCtx)
		if preflightErr != nil {
			result := newDirectRunFailureResult(root, resolvedTarget, mode, commandStarted, preflightErr)
			result.RepositoryChanges = &repositoryResult
			if jsonOut {
				if writeErr := writeJSON(a.Stdout, result); writeErr != nil {
					return writeErr
				}
			}
			return preflightErr
		}
	}
	progressCtx, stopProgress := context.WithCancel(context.Background())
	var progressWG sync.WaitGroup
	if mode == api.ModeCI && jsonOut {
		// Subscribe synchronously so a fast run cannot publish run_started
		// before the progress goroutine has been scheduled. Direct CI output is
		// lossless because stderr is the only live execution record.
		progressEvents := eng.SubscribeEventsLossless()
		progressWG.Add(1)
		go func() {
			defer progressWG.Done()
			streamCIProgress(progressCtx, a.Stderr, progressEvents)
		}()
	}
	outcome, runErr := eng.Run(runCtx, engine.Request{
		Target:               resolvedTarget,
		Worktree:             root,
		Mode:                 mode,
		MaxParallel:          maxParallel,
		CacheKeyManifestPath: cacheKeyManifest,
	})
	stopProgress()
	progressWG.Wait()
	if repairRunner != nil {
		if outcome == nil {
			if runErr != nil {
				repositoryResult := repairRunner.SkippedDAGFailure()
				result := newDirectRunFailureResult(root, resolvedTarget, mode, commandStarted, runErr)
				result.RepositoryChanges = &repositoryResult
				if jsonOut {
					if writeErr := writeJSON(a.Stdout, result); writeErr != nil {
						return writeErr
					}
				}
			}
			return runErr
		}
		if runErr != nil || !outcome.Result.Success {
			repositoryResult := repairRunner.SkippedDAGFailure()
			outcome.Result.RepositoryChanges = &repositoryResult
		} else {
			repairStarted := time.Now()
			repositoryResult, repairErr := repairRunner.Apply(runCtx)
			outcome.Result.RepositoryChanges = &repositoryResult
			outcome.Result.DurationMs += time.Since(repairStarted).Milliseconds()
			outcome.Result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			if repairErr != nil {
				outcome.Result.Success = false
				outcome.Result.Error = repairErr.Error()
				runErr = repairErr
			}
		}
	}
	if outcome != nil {
		if jsonOut {
			if err := writeJSON(a.Stdout, outcome.Result); err != nil {
				return err
			}
			return runErr
		}
		_, _ = fmt.Fprintf(a.Stdout, "target=%s instance=%s success=%v cache_hits=%d", outcome.Result.Target, outcome.Result.InstanceID, outcome.Result.Success, len(outcome.Result.CacheHits))
		if outcome.Result.RepositoryChanges != nil {
			_, _ = fmt.Fprintf(a.Stdout, " repository_status=%s commit=%s push_attempted=%t push_succeeded=%t", outcome.Result.RepositoryChanges.Status, outcome.Result.RepositoryChanges.CommitSHA, outcome.Result.RepositoryChanges.PushAttempted, outcome.Result.RepositoryChanges.PushSucceeded)
		}
		_, _ = fmt.Fprintln(a.Stdout)
	}
	return runErr
}

func newDirectRunFailureResult(worktree, target string, mode api.RunMode, started time.Time, runErr error) api.RunResult {
	instanceID, _, _ := instance.IDForWorktree(worktree)
	finished := time.Now().UTC()
	durationMs := finished.Sub(started).Milliseconds()
	if durationMs == 0 && !finished.Before(started) {
		durationMs = 1
	}
	result := api.RunResult{
		Target:          target,
		Mode:            mode,
		InstanceID:      instanceID,
		Success:         false,
		DurationMs:      durationMs,
		Error:           runErr.Error(),
		FailureExcerpts: []api.FailureExcerpt{},
		Nodes:           []api.NodeStatus{},
		CacheHits:       []string{},
		CacheMisses:     []string{},
		StartedAt:       started.Format(time.RFC3339),
		FinishedAt:      finished.Format(time.RFC3339),
	}
	result.ResourceConflict = executionconflict.Details(runErr)
	if result.ResourceConflict != nil {
		result.Code = "resource_conflict"
	}
	return result
}

// Admission can fail before a command result exists. Keep ownership details
// available on stdout so callers can distinguish contention from task failure.
func (a *App) reportResourceConflict(err error, jsonOut bool, extraFields ...map[string]any) error {
	detail := executionconflict.Details(err)
	if jsonOut && detail != nil {
		payload := map[string]any{"success": false, "error": err.Error(), "code": "resource_conflict", "resourceConflict": detail}
		for _, fields := range extraFields {
			for key, value := range fields {
				if value != nil {
					payload[key] = value
				}
			}
		}
		if writeErr := writeJSON(a.Stdout, payload); writeErr != nil {
			return writeErr
		}
	}
	return err
}

func streamCIProgress(ctx context.Context, out io.Writer, events <-chan api.Event) {
	write := func(evt api.Event) {
		switch evt.Type {
		case api.EventRunStarted:
			_, _ = fmt.Fprintf(out, "[devflow] run %s started\n", evt.Target)
		case api.EventTaskState:
			if evt.Error != "" {
				_, _ = fmt.Fprintf(out, "[devflow] task %s: %s: %s\n", evt.Task, evt.State, evt.Error)
			} else {
				_, _ = fmt.Fprintf(out, "[devflow] task %s: %s\n", evt.Task, evt.State)
			}
		case api.EventCacheHit:
			_, _ = fmt.Fprintf(out, "[devflow] cache %s: hit\n", evt.Task)
		case api.EventCacheMiss:
			_, _ = fmt.Fprintf(out, "[devflow] cache %s: miss\n", evt.Task)
		case api.EventLogLine:
			_, _ = fmt.Fprintf(out, "[devflow] %s %s: %s\n", evt.Task, evt.Stream, evt.Line)
		case api.EventRunFinished:
			_, _ = fmt.Fprintf(out, "[devflow] run %s finished success=%t\n", evt.Target, evt.Success != nil && *evt.Success)
		}
	}
	for {
		select {
		case evt, ok := <-events:
			if !ok {
				return
			}
			write(evt)
		case <-ctx.Done():
			for {
				select {
				case evt, ok := <-events:
					if !ok {
						return
					}
					write(evt)
				default:
					return
				}
			}
		}
	}
}

func (a *App) runViaDaemon(target string, jsonOut bool, worktreeFlag, projectName string, detach bool, maxParallel int) error {
	root, err := resolveWorktree(worktreeFlag)
	if err != nil {
		return a.reportResourceConflict(err, jsonOut)
	}
	client, _, err := daemon.Ensure(context.Background(), root, projectName)
	if err != nil {
		return a.reportResourceConflict(err, jsonOut)
	}
	resp, err := client.Call(context.Background(), daemon.Request{
		Action:      daemon.ActionRun,
		Project:     projectName,
		Target:      target,
		Mode:        api.ModeDev,
		MaxParallel: maxParallel,
		Detach:      detach,
	})
	if err != nil {
		return a.reportResourceConflict(err, jsonOut)
	}
	if detach {
		if resp.Started != nil {
			payload := resp.Started
			if jsonOut {
				return writeJSON(a.Stdout, payload)
			}
			_, _ = fmt.Fprintf(a.Stdout, "daemon instance=%s pid=%d target=%s\n", resp.Started.InstanceID, resp.Started.DaemonPID, resp.Started.Target)
		}
		return nil
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
		return a.reportResourceConflict(err, jsonOut)
	}
	client, _, err := daemon.Ensure(context.Background(), root, projectName)
	if err != nil {
		return a.reportResourceConflict(err, jsonOut)
	}
	resp, err := client.Call(context.Background(), daemon.Request{
		Action:      daemon.ActionWatch,
		Project:     projectName,
		Target:      target,
		MaxParallel: maxParallel,
	})
	if err != nil {
		return a.reportResourceConflict(err, jsonOut)
	}
	if resp.Started == nil {
		return fmt.Errorf("daemon did not return watch start metadata")
	}
	payload := resp.Started
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
	client, _, err := daemon.Ensure(context.Background(), root, *projectName)
	if err != nil {
		return a.reportResourceConflict(err, *jsonOut)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+5*time.Second)
	defer cancel()
	callStarted := time.Now()
	resp, callErr := client.Call(ctx, daemon.Request{
		Action:      daemon.ActionFlush,
		Project:     *projectName,
		Target:      target,
		TimeoutMs:   timeout.Milliseconds(),
		MaxParallel: *maxParallel,
	})
	if resp.Flush != nil {
		result := preserveFlushCallError(*resp.Flush, callErr, root, id, *projectName, target, time.Since(callStarted))
		return a.finishFlush(result, *jsonOut)
	}
	if callErr == nil {
		callErr = fmt.Errorf("daemon flush response did not include a result")
	}
	result := preserveFlushCallError(api.FlushResult{}, callErr, root, id, *projectName, target, time.Since(callStarted))
	return a.finishFlush(result, *jsonOut)
}

func preserveFlushCallError(result api.FlushResult, callErr error, worktree, instanceID, projectName, target string, elapsed time.Duration) api.FlushResult {
	if detail := executionconflict.Details(callErr); detail != nil {
		result.Code = "resource_conflict"
		result.ResourceConflict = detail
	}
	if callErr == nil || result.Success || len(result.Issues) > 0 {
		return result
	}
	// A transport failure may arrive before any result. Preserve invocation
	// context so callers can identify the failed operation without stderr.
	if result.Worktree == "" {
		result.Worktree = worktree
	}
	if result.InstanceID == "" {
		result.InstanceID = instanceID
	}
	if result.Project == "" {
		result.Project = projectName
	}
	if result.Target == "" {
		result.Target = target
	}
	if result.Mode == "" {
		result.Mode = api.ModeWatch
	}
	if result.DurationMs == 0 && elapsed >= 0 {
		result.DurationMs = max(elapsed.Milliseconds(), 1)
	}
	if result.UpdatedAt.IsZero() {
		result.UpdatedAt = time.Now().UTC()
	}
	result.Issues = append(result.Issues, api.FlushIssue{
		Kind:    "daemon_error",
		Message: callErr.Error(),
	})
	return result
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
	preview := fs.Bool("preview", false, "preview lifecycle scope without changing processes")
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
		return a.reportResourceConflict(err, *jsonOut)
	}
	resp, runErr := client.Call(context.Background(), daemon.Request{
		Action:      daemon.ActionRestart,
		Project:     *projectName,
		Task:        task,
		Upstream:    *upstream,
		Downstream:  *downstream,
		MaxParallel: *maxParallel,
		Preview:     *preview,
	})
	if detail := executionconflict.Details(runErr); detail != nil {
		if *jsonOut {
			if err := writeJSON(a.Stdout, map[string]any{"success": false, "code": "resource_conflict", "error": runErr.Error(), "resourceConflict": detail, "lifecycle": resp.Lifecycle}); err != nil {
				return err
			}
		}
		return runErr
	}

	if *preview {
		if resp.Lifecycle == nil {
			if runErr != nil {
				return runErr
			}
			return fmt.Errorf("daemon returned no lifecycle plan")
		}
		if *jsonOut {
			if err := writeJSON(a.Stdout, resp.Lifecycle); err != nil {
				return err
			}
		} else {
			writeLifecyclePlanText(a.Stdout, resp.Lifecycle.Plan)
		}
		return runErr
	}
	if resp.Run != nil {
		resp.Run.Lifecycle = resp.Lifecycle
		if *jsonOut {
			if err := writeJSON(a.Stdout, resp.Run); err != nil {
				return err
			}
			return runErr
		}
		_, _ = fmt.Fprintf(a.Stdout, "restarted=%s success=%v cache_hits=%d\n", task, resp.Run.Success, len(resp.Run.CacheHits))
	} else if *jsonOut && resp.Lifecycle != nil {
		if err := writeJSON(a.Stdout, resp.Lifecycle); err != nil {
			return err
		}
		return runErr
	} else if runErr == nil {
		if *jsonOut {
			payload := map[string]any{"restarted": task, "success": true}
			if resp.Lifecycle != nil {
				payload["affected"] = resp.Lifecycle.Affected
				payload["plan"] = resp.Lifecycle.Plan
				payload["processes"] = resp.Lifecycle.Processes
			}
			return writeJSON(a.Stdout, payload)
		}
		affected := []string{task}
		if resp.Lifecycle != nil {
			affected = resp.Lifecycle.Affected
		}
		_, _ = fmt.Fprintf(a.Stdout, "restarted: %s\n", strings.Join(affected, ", "))
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
	preview := fs.Bool("preview", false, "preview lifecycle scope without changing processes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*all && *task == "" {
		return fmt.Errorf("usage: devflow stop --task <name> | --all")
	}
	root, id, err := resolveInstance(*worktree, *instanceID)
	if err != nil {
		return a.reportResourceConflict(err, *jsonOut)
	}
	_ = id
	client, _, err := daemon.Ensure(context.Background(), root, "")
	if err != nil {
		return a.reportResourceConflict(err, *jsonOut)
	}
	resp, err := client.Call(context.Background(), daemon.Request{Action: daemon.ActionStop, All: *all, Task: *task, Preview: *preview})
	if executionconflict.Details(err) != nil {
		fields := map[string]any{"instanceId": id, "lifecycle": resp.Lifecycle}
		if resp.Stop != nil {
			fields["stopped"] = resp.Stop.Stopped
		}
		return a.reportResourceConflict(err, *jsonOut, fields)
	}
	if err != nil && resp.Stop == nil && resp.Lifecycle == nil {
		return err
	}
	if err == nil && *all && !*preview {
		waitForDaemonDisconnect(client, 3*time.Second)
	}
	stopped := []string{}
	if resp.Stop != nil {
		stopped = resp.Stop.Stopped
	}
	payload := map[string]any{
		"instanceId": id,
		"stopped":    stopped,
	}
	if resp.Lifecycle != nil {
		payload["lifecycle"] = resp.Lifecycle
		payload["plan"] = resp.Lifecycle.Plan
	}
	if *jsonOut {
		if writeErr := writeJSON(a.Stdout, payload); writeErr != nil {
			return writeErr
		}
		return err
	}
	if *preview && resp.Lifecycle != nil {
		writeLifecyclePlanText(a.Stdout, resp.Lifecycle.Plan)
		return nil
	}
	_, _ = fmt.Fprintf(a.Stdout, "stopped: %s\n", strings.Join(stopped, ", "))
	if resp.Lifecycle != nil {
		for _, issue := range resp.Lifecycle.Issues {
			_, _ = fmt.Fprintf(a.Stdout, "not stopped: %s: %s\n", issue.Resource, issue.Reason)
		}
	}
	return err
}

func writeLifecyclePlanText(output io.Writer, plan api.LifecyclePlan) {
	_, _ = fmt.Fprintf(output, "action=%s task=%s target=%s\n", plan.RequestedAction, plan.SelectedTask, plan.SelectedTarget)
	_, _ = fmt.Fprintf(output, "invalidate=%s\n", strings.Join(plan.TasksToInvalidate, ","))
	_, _ = fmt.Fprintf(output, "stop=%s\n", strings.Join(plan.ProcessesToStop, ","))
	_, _ = fmt.Fprintf(output, "execute=%s\n", strings.Join(plan.TasksToExecute, ","))
	_, _ = fmt.Fprintf(output, "preserve=%s\n", strings.Join(plan.ServicesToPreserve, ","))
	_, _ = fmt.Fprintf(output, "restart=%s confirmation_recommended=%v\n", strings.Join(plan.ServicesToRestart, ","), plan.ConfirmationRecommended)
}

func (a *App) actionCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: devflow action <list|run>")
	}
	switch args[0] {
	case "list":
		return a.actionListCmd(args[1:])
	case "run":
		return a.actionRunCmd(args[1:])
	default:
		return fmt.Errorf("usage: devflow action <list|run>")
	}
}

func (a *App) actionListCmd(args []string) error {
	fs := flag.NewFlagSet("action list", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "emit stable JSON output")
	worktree := fs.String("worktree", "", "project worktree path")
	projectName := fs.String("project", defaultProject(), "registered project adapter name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return err
	}
	client, _, err := daemon.Ensure(context.Background(), root, *projectName)
	if err != nil {
		return err
	}
	resp, err := client.Call(context.Background(), daemon.Request{
		Action:  daemon.ActionListActions,
		Project: *projectName,
	})
	if err != nil {
		return err
	}
	if resp.Actions == nil {
		return fmt.Errorf("daemon did not return action list")
	}
	if *jsonOut {
		return writeJSON(a.Stdout, resp.Actions)
	}
	for _, action := range resp.Actions.Actions {
		label := action.Label
		if label == "" {
			label = action.ID
		}
		_, _ = fmt.Fprintf(a.Stdout, "%s\t%s\t%s\n", action.ID, action.Kind, label)
	}
	return nil
}

func (a *App) actionRunCmd(args []string) error {
	actionID := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		actionID = args[0]
		args = args[1:]
	}
	inputs := map[string]string{}
	inputFlags := kvFlags{}
	fs := flag.NewFlagSet("action run", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "emit stable JSON output")
	worktree := fs.String("worktree", "", "project worktree path")
	projectName := fs.String("project", defaultProject(), "registered project adapter name")
	kind := fs.String("kind", "", "action kind to run when no action ID is provided")
	component := fs.String("component", "", "component ID used to disambiguate action kind")
	name := fs.String("name", "", "common input value named \"name\"")
	fs.Var(&inputFlags, "input", "action input as key=value; may be repeated")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if actionID == "" && fs.NArg() > 0 {
		actionID = fs.Arg(0)
	}
	if *name != "" {
		inputs["name"] = *name
	}
	for key, value := range inputFlags.values {
		inputs[key] = value
	}
	if fs.NArg() > 1 && inputs["name"] == "" {
		inputs["name"] = fs.Arg(1)
	}
	if actionID == "" && *kind == "" {
		return fmt.Errorf("usage: devflow action run <action-id> [--input key=value]")
	}
	return a.runAction(actionID, *kind, *component, inputs, *jsonOut, *worktree, *projectName)
}

func (a *App) migrationCmd(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return fmt.Errorf("usage: devflow migration create <name>")
	}
	return a.migrationCreateCmd(args[1:])
}

func (a *App) migrationCreateCmd(args []string) error {
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("migration create", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "emit stable JSON output")
	worktree := fs.String("worktree", "", "project worktree path")
	projectName := fs.String("project", defaultProject(), "registered project adapter name")
	component := fs.String("component", "", "component ID used when several migration systems exist")
	force := fs.Bool("force", false, "allow component-specific force/accept-warning behavior when declared")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if name == "" {
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: devflow migration create <name>")
		}
		name = fs.Arg(0)
	}
	inputs := map[string]string{"name": name}
	if *force {
		inputs["force"] = "true"
	}
	return a.runAction("", database.ActionMigrationCreate, *component, inputs, *jsonOut, *worktree, *projectName)
}

func (a *App) runAction(actionID, kind, component string, inputs map[string]string, jsonOut bool, worktreeFlag, projectName string) error {
	root, err := resolveWorktree(worktreeFlag)
	if err != nil {
		return a.reportResourceConflict(err, jsonOut)
	}
	client, _, err := daemon.Ensure(context.Background(), root, projectName)
	if err != nil {
		return a.reportResourceConflict(err, jsonOut)
	}
	resp, callErr := client.Call(context.Background(), daemon.Request{
		Action:       daemon.ActionRunAction,
		Project:      projectName,
		ActionID:     actionID,
		ActionKind:   kind,
		Component:    component,
		Inputs:       inputs,
		StreamEvents: !jsonOut,
	}, func(evt api.Event) {
		if evt.Type == api.EventLogLine && !jsonOut {
			if evt.Task == "daemon" && evt.Line != "" {
				_, _ = fmt.Fprintf(a.Stderr, "%s\n", evt.Line)
			}
		}
	})
	if executionconflict.Details(callErr) != nil {
		return a.reportResourceConflict(callErr, jsonOut, map[string]any{"actionResult": resp.ActionResult})
	}
	if resp.ActionResult != nil {
		if jsonOut {
			if err := writeJSON(a.Stdout, resp.ActionResult); err != nil {
				return err
			}
			return callErr
		}
		_, _ = fmt.Fprintf(a.Stdout, "action=%s status=%s\n", resp.ActionResult.ActionID, resp.ActionResult.Status)
		return callErr
	}
	return callErr
}

func (a *App) cacheCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: devflow cache <status|key|path|invalidate|gc>")
	}
	switch args[0] {
	case "status":
		return a.cacheStatusCmd(args[1:])
	case "key":
		return a.cacheKeyCmd(args[1:])
	case "path":
		return a.cachePathCmd(args[1:])
	case "invalidate":
		return a.cacheInvalidateCmd(args[1:])
	case "gc":
		return a.cacheGCCmd(args[1:])
	default:
		return fmt.Errorf("usage: devflow cache <status|key|path|invalidate|gc>")
	}
}

func (a *App) cacheKeyCmd(args []string) error {
	fs := flag.NewFlagSet("cache key", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "emit stable JSON output")
	worktree := fs.String("worktree", "", "project worktree path")
	projectName := fs.String("project", "", "project adapter name")
	target := fs.String("target", "", "target or task whose cache key should be computed")
	manifestOut := fs.String("manifest-out", "", "write an owner-only cache-key manifest for immediate run reuse")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*target) == "" {
		return fmt.Errorf("cache key requires --target")
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return err
	}
	p, err := resolvedProject(*projectName, root)
	if err != nil {
		return err
	}
	executionProject, resolvedTarget, err := project.ResolveExecutionProject(p, *target)
	if err != nil {
		return err
	}
	lease, err := executionstate.Acquire(root, execution.Owner{Target: resolvedTarget, Kind: "cache_key"})
	if err != nil {
		return a.reportResourceConflict(err, *jsonOut)
	}
	defer lease.Release()
	eng, err := engine.New(executionProject, root)
	if err != nil {
		return err
	}
	result, manifest, err := eng.CacheKeyWithManifest(execution.ContextWithLease(context.Background(), lease), engine.Request{Target: resolvedTarget, Worktree: root, Mode: api.ModeCI})
	if err != nil {
		return err
	}
	if *manifestOut != "" {
		manifestPath, err := filepath.Abs(*manifestOut)
		if err != nil {
			return fmt.Errorf("resolve cache key manifest path: %w", err)
		}
		if err := engine.WriteCacheKeyManifest(manifestPath, manifest); err != nil {
			return fmt.Errorf("write cache key manifest: %w", err)
		}
		result.ManifestPath = manifestPath
	}
	if *jsonOut {
		return writeJSON(a.Stdout, result)
	}
	_, _ = fmt.Fprintln(a.Stdout, result.Key)
	return nil
}

func (a *App) cachePathCmd(args []string) error {
	fs := flag.NewFlagSet("cache path", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "emit stable JSON output")
	worktree := fs.String("worktree", "", "project worktree path")
	projectName := fs.String("project", "", "project adapter name")
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
	result := api.CachePathResult{
		Project:       p.Name(),
		Namespace:     store.Namespace,
		CacheRoot:     instance.CacheRoot(),
		NamespacePath: store.EntriesRoot(),
	}
	if *jsonOut {
		return writeJSON(a.Stdout, result)
	}
	_, _ = fmt.Fprintln(a.Stdout, result.NamespacePath)
	return nil
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
	lease, err := executionstate.Acquire(root, execution.Owner{Target: *task, Kind: "cache_invalidate"})
	if err != nil {
		return a.reportResourceConflict(err, *jsonOut)
	}
	defer lease.Release()
	store := cache.NewNamespaced(instance.CacheRoot(), project.CacheNamespace(p))
	if err := store.Invalidate(*task); err != nil {
		return err
	}
	instanceID, real, err := instance.IDForWorktree(root)
	if err != nil {
		return err
	}
	if *task == "" {
		if err := instance.RemoveTaskStamps(real, instanceID); err != nil {
			return err
		}
	} else if err := instance.RemoveTaskStamp(real, instanceID, *task); err != nil {
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
	out, err := statusResult(root, id)
	if err != nil {
		return err
	}
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
	if out.Daemon != nil {
		state := "stopped"
		if out.Daemon.Alive {
			state = "running"
		}
		_, _ = fmt.Fprintf(a.Stdout, "daemon: %s pid=%d log=%s\n", state, out.Daemon.PID, out.Daemon.LogPath)
	}
	_, _ = fmt.Fprintln(a.Stdout)
	for _, node := range out.Nodes {
		_, _ = fmt.Fprintf(a.Stdout, "%-20s %-10s %s\n", node.Name, node.Kind, node.State)
	}
	return nil
}

func statusResult(root, instanceID string) (api.StatusResult, error) {
	if client, err := daemon.Dial(root); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		resp, callErr := client.Call(ctx, daemon.Request{Action: daemon.ActionStatus})
		if callErr == nil && resp.Status != nil {
			return *resp.Status, nil
		}
	}
	inst, err := instance.Load(root, instanceID)
	if err != nil {
		return api.StatusResult{}, err
	}
	state, err := instance.LoadStatus(root, instanceID)
	if err != nil {
		if !os.IsNotExist(err) {
			return api.StatusResult{}, err
		}
		state = &instance.State{Target: inst.LastRun.Target, Mode: inst.LastRun.Mode, Nodes: map[string]api.NodeStatus{}}
	}
	if state.Target == "" && inst.LastRun.Target != "" {
		state.Target = inst.LastRun.Target
	}
	if state.Mode == "" && inst.LastRun.Mode != "" {
		state.Mode = inst.LastRun.Mode
	}
	nodes := make([]api.NodeStatus, 0, len(state.Nodes))
	for _, node := range state.Nodes {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return api.StatusResult{
		InstanceID: instanceID,
		Worktree:   root,
		Target:     state.Target,
		Mode:       state.Mode,
		UpdatedAt:  state.UpdatedAt,
		Ports:      inst.Ports,
		DB:         instance.DisplayDB(inst.DB),
		URLs:       daemon.InstanceURLs(inst),
		Daemon:     daemon.DaemonStatus(inst),
		Nodes:      nodes,
	}, nil
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
	strict := fs.Bool("strict", false, "return a nonzero exit when any doctor check fails")
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
	requiredEnv := project.RequiredEnvsFor(p)
	executionProject := p
	if strings.TrimSpace(*target) != "" {
		cliScope = "target"
		executionProject, resolvedTarget, requiredCLIs, err = requiredCLIsForTargetScope(p, *target)
		if err != nil {
			return err
		}
		requiredEnv, err = project.RequiredEnvsForTarget(executionProject, resolvedTarget)
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
	if len(requiredEnv) > 0 {
		cfg, cfgErr := executionProject.ConfigureInstance(context.Background(), root)
		if cfgErr != nil {
			return cfgErr
		}
		missing := make([]string, 0)
		for _, key := range requiredEnv {
			status := api.DoctorEnvStatus{Name: key}
			if value, ok := os.LookupEnv(key); ok {
				status.Source = "process"
				status.Set = strings.TrimSpace(value) != ""
			} else if strings.TrimSpace(cfg.Env[key]) != "" {
				status.Source = "project"
				status.Set = true
			}
			if !status.Set {
				result.ChecksPassed = false
				missing = append(missing, key)
			}
			result.RequiredEnv = append(result.RequiredEnv, status)
		}
		if len(missing) == 0 {
			result.Checks = append(result.Checks, fmt.Sprintf("required_env: ok (%d)", len(requiredEnv)))
		} else {
			result.Warnings = append(result.Warnings, "missing required env: "+strings.Join(missing, ", "))
		}
	} else if cliScope == "target" {
		result.Checks = append(result.Checks, "required_env: ok (0)")
	}
	if *jsonOut {
		if err := writeJSON(a.Stdout, result); err != nil {
			return err
		}
	} else {
		for _, check := range result.Checks {
			_, _ = fmt.Fprintln(a.Stdout, check)
		}
		for _, warning := range result.Warnings {
			_, _ = fmt.Fprintln(a.Stdout, "warning: "+warning)
		}
	}
	if *strict && !result.ChecksPassed {
		return fmt.Errorf("doctor checks failed")
	}
	return nil
}

func (a *App) clisCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: devflow clis <status|install>")
	}
	switch args[0] {
	case "status":
		return a.clisStatusCmd(args[1:])
	case "install":
		return a.clisInstallCmd(args[1:])
	default:
		return fmt.Errorf("usage: devflow clis <status|install>")
	}
}

func (a *App) clisStatusCmd(args []string) error {
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

func (a *App) clisInstallCmd(args []string) error {
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
	if strings.TrimSpace(*instanceID) == "" {
		root, err := resolveWorktree(*worktree)
		if err != nil {
			return err
		}
		return a.launchDefaultTUI(root)
	}
	return tui.Run(tui.Options{
		Worktree:   *worktree,
		InstanceID: *instanceID,
		Output:     a.Stdout,
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
	_, _ = fmt.Fprintf(a.Stderr, "[devflow] upgrade started target=%s command=%q\n", target, strings.Join(command, " "))
	var output bytes.Buffer
	if *jsonOut {
		// Preserve one final JSON document on stdout while making the child
		// command observable during long downloads and compilation.
		combined := io.MultiWriter(a.Stderr, &output)
		cmd.Stdout = combined
		cmd.Stderr = combined
	} else {
		cmd.Stdout = a.Stdout
		cmd.Stderr = a.Stderr
	}
	err := cmd.Run()
	cacheCleared := false
	if err == nil {
		// Upgrades invalidate artifacts so cache changes need no migration path.
		if clearErr := fsutil.RemoveAllWritable(instance.CacheRoot()); clearErr != nil {
			err = fmt.Errorf("installed Devflow, but clearing task cache: %w", clearErr)
		} else {
			cacheCleared = true
		}
	}
	duration := time.Since(started)
	_, _ = fmt.Fprintf(a.Stderr, "[devflow] upgrade finished success=%t duration_ms=%d\n", err == nil, duration.Milliseconds())
	result := api.UpgradeResult{
		Command:       command,
		Package:       version.CommandPackage,
		VersionTarget: target,
		Success:       err == nil,
		CacheCleared:  cacheCleared,
		DurationMs:    duration.Milliseconds(),
		Output:        strings.TrimSpace(output.String()),
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
	return filepath.Join(binDir, devflowExecutableName()), nil
}

func devflowExecutableName() string {
	if runtime.GOOS == "windows" {
		return "devflow.exe"
	}
	return "devflow"
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

type kvFlags struct {
	values map[string]string
}

type repeatedStringFlags []string

func (f *repeatedStringFlags) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *repeatedStringFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func (f *kvFlags) String() string {
	if f == nil || len(f.values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(f.values))
	for key := range f.values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+f.values[key])
	}
	return strings.Join(parts, ",")
}

func (f *kvFlags) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return fmt.Errorf("expected key=value")
	}
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[key] = val
	return nil
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

func resolveLogPath(worktree, instanceID, task string) (string, error) {
	if task != "daemon" {
		return instance.LogPath(worktree, instanceID, task), nil
	}
	ref, err := instance.LoadDaemon(worktree, instanceID)
	if err != nil {
		return "", err
	}
	if ref.LogPath != "" {
		return ref.LogPath, nil
	}
	return filepath.Join(worktree, ".devflow", "logs", instanceID, "daemon.log"), nil
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

func waitForDaemonDisconnect(client *daemon.Client, timeout time.Duration) {
	if client == nil || timeout <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		err := client.Ping(ctx)
		cancel()
		if err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitForInitialStatus(worktree, instanceID, target string, mode api.RunMode, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if state, err := instance.LoadStatus(worktree, instanceID); err == nil && statusMatchesInitialRun(state, target, mode) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func statusMatchesInitialRun(state *instance.State, target string, mode api.RunMode) bool {
	if state == nil {
		return false
	}
	if strings.TrimSpace(target) != "" && state.Target != target {
		return false
	}
	if mode != "" && state.Mode != mode {
		return false
	}
	return len(state.Nodes) > 0
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
