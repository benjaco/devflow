package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/daemon"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

type githubCLIProject struct {
	name              string
	tasks             []project.Task
	roots             []string
	githubEnvironment string
}

func (p *githubCLIProject) Name() string          { return p.name }
func (p *githubCLIProject) Tasks() []project.Task { return p.tasks }
func (p *githubCLIProject) Targets() []project.Target {
	return []project.Target{{Name: "verify", RootTasks: p.roots}}
}
func (p *githubCLIProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	// Presentation follows the invoking process, even when task env disagrees.
	return project.InstanceConfig{Env: map[string]string{"GITHUB_ACTIONS": p.githubEnvironment}}, nil
}

func githubCLIFixture(t *testing.T, tasks []project.Task, roots ...string) (*githubCLIProject, string) {
	t.Helper()
	p := &githubCLIProject{name: strings.ReplaceAll(t.Name(), "/", "-"), tasks: tasks, roots: roots, githubEnvironment: "false"}
	project.Register(p)
	return p, t.TempDir()
}

func githubCLIRun(ctx context.Context, p *githubCLIProject, worktree string, stderr io.Writer, extra ...string) (api.RunResult, string, error) {
	var stdout bytes.Buffer
	app := &App{Context: ctx, Stdout: &stdout, Stderr: stderr}
	args := []string{"run", "verify", "--ci", "--project", p.Name(), "--worktree", worktree}
	err := app.Run(append(args, extra...))
	var result api.RunResult
	if strings.Contains(strings.Join(extra, " "), "--json") {
		if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
			return result, stdout.String(), fmt.Errorf("final stdout is not one JSON result: %w (execution: %v)", decodeErr, err)
		}
	}
	return result, stdout.String(), err
}

func TestGitHubCIAutomaticSelectionAndProgressControls(t *testing.T) {
	for _, environment := range []string{"true", "false", "", "<unset>"} {
		for _, jsonOut := range []bool{false, true} {
			for _, progress := range []string{"logs", "states", "quiet"} {
				t.Run(fmt.Sprintf("%s/json=%t/%s", environment, jsonOut, progress), func(t *testing.T) {
					t.Setenv("GITHUB_ACTIONS", environment)
					if environment == "<unset>" {
						if err := os.Unsetenv("GITHUB_ACTIONS"); err != nil {
							t.Fatal(err)
						}
					}
					t.Setenv("GITHUB_STEP_SUMMARY", "")
					var executions atomic.Int32
					p, root := githubCLIFixture(t, []project.Task{{Name: "check", Kind: project.KindOnce, Run: func(_ context.Context, rt *project.Runtime) error {
						executions.Add(1)
						rt.EmitLogLine("stdout", "selection-output-marker")
						return nil
					}}}, "check")
					args := []string{"--progress", progress}
					if jsonOut {
						args = append(args, "--json")
					}
					var stderr bytes.Buffer
					result, stdout, err := githubCLIRun(context.Background(), p, root, &stderr, args...)
					if err != nil {
						t.Fatalf("run: %v\nstdout=%s\nstderr=%s", err, stdout, &stderr)
					}
					if executions.Load() != 1 || (jsonOut && (!result.Success || result.Mode != api.ModeCI)) {
						t.Fatalf("presentation changed execution: count=%d result=%+v", executions.Load(), result)
					}
					grouped := environment == "true" && progress == "logs"
					if got := strings.Contains(stderr.String(), "::group::check | SUCCESS | "); got != grouped {
						t.Fatalf("automatic grouped=%t, want %t; stdout=%s stderr=%s", got, grouped, stdout, &stderr)
					}
					if progress == "quiet" && stderr.Len() != 0 {
						t.Fatalf("quiet emitted progress: %s", &stderr)
					}
					if progress == "states" && strings.Contains(stderr.String(), "selection-output-marker") {
						t.Fatalf("states replayed logs: %s", &stderr)
					}
					if environment == "true" && progress != "quiet" && !strings.Contains(stderr.String(), "[devflow]") {
						t.Fatalf("missing live lifecycle progress: %s", &stderr)
					}
					if grouped && strings.Count(stderr.String(), "selection-output-marker") != 1 {
						t.Fatalf("task output lost or duplicated: %s", &stderr)
					}
				})
			}
		}
	}
}

type githubCLIGroup struct{ title, body string }

func githubCLIParseGroups(t *testing.T, output string) []githubCLIGroup {
	t.Helper()
	var groups []githubCLIGroup
	var active *githubCLIGroup
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "::group::"):
			if active != nil {
				t.Fatalf("nested or interleaved groups: %s", output)
			}
			active = &githubCLIGroup{title: strings.TrimPrefix(line, "::group::")}
		case line == "::endgroup::":
			if active == nil {
				t.Fatalf("unmatched group end: %s", output)
			}
			groups = append(groups, *active)
			active = nil
		default:
			if active != nil {
				if strings.HasPrefix(line, "[devflow]") || strings.HasPrefix(line, "::error") {
					t.Fatalf("lifecycle interleaved with retained log: %s", output)
				}
				active.body += line + "\n"
			}
		}
	}
	if active != nil {
		t.Fatalf("unterminated group: %s", output)
	}
	return groups
}

func TestGitHubCIParallelSharedDependencyAndRecordedDurations(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	summary := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)
	started := make(chan string, 2)
	release := make(chan struct{})
	var sharedExecutions atomic.Int32
	tasks := []project.Task{{Name: "shared", Kind: project.KindOnce, Run: func(_ context.Context, rt *project.Runtime) error {
		sharedExecutions.Add(1)
		rt.EmitLogLine("stdout", "shared-only-marker")
		return nil
	}}}
	for _, name := range []string{"backend:build", "frontend:lint"} {
		tasks = append(tasks, project.Task{Name: name, Kind: project.KindOnce, Deps: []string{"shared"}, Run: func(ctx context.Context, rt *project.Runtime) error {
			rt.EmitLogLine("stdout", name+"-only-marker")
			started <- name
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}})
	}
	tasks = append(tasks, project.Task{Name: "all", Kind: project.KindGroup, Deps: []string{"backend:build", "frontend:lint"}})
	p, root := githubCLIFixture(t, tasks, "all")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	type completion struct {
		result api.RunResult
		stdout string
		err    error
	}
	done := make(chan completion, 1)
	go func() {
		result, stdout, err := githubCLIRun(ctx, p, root, &stderr, "--json", "--max-parallel", "2")
		done <- completion{result, stdout, err}
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case value := <-done:
			t.Fatalf("finished before both parallel tasks reached barrier: %v\n%s", value.err, &stderr)
		case <-ctx.Done():
			t.Fatal("two independent tasks did not overlap at the barrier")
		}
	}
	close(release)
	value := <-done
	if value.err != nil || !value.result.Success || sharedExecutions.Load() != 1 {
		t.Fatalf("parallel run changed: %v result=%s shared=%d", value.err, value.stdout, sharedExecutions.Load())
	}
	groups := githubCLIParseGroups(t, stderr.String())
	if len(groups) != 3 {
		t.Fatalf("want three actual attempts and no group-node execution, got %d:\n%s", len(groups), &stderr)
	}
	record, err := instance.LoadRun(root, value.result.InstanceID, value.result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		parts := strings.Split(group.title, " | ")
		if len(parts) != 3 || parts[1] != "SUCCESS" {
			t.Fatalf("missing final status or execution duration: %q", group.title)
		}
		if strings.Count(stderr.String(), parts[0]+"-only-marker") != 1 || !strings.Contains(group.body, parts[0]+"-only-marker") {
			t.Fatalf("wrong task attribution: %+v", group)
		}
		for _, other := range []string{"shared", "backend:build", "frontend:lint"} {
			if other != parts[0] && strings.Contains(group.body, other+"-only-marker") {
				t.Fatalf("interleaved task logs: %+v", group)
			}
		}
		duration, err := time.ParseDuration(parts[2])
		if err != nil {
			t.Fatalf("non-duration title %q: %v", group.title, err)
		}
		for _, attempt := range record.Attempts {
			if attempt.Task == parts[0] {
				difference := duration - attempt.FinishedAt.Sub(attempt.StartedAt)
				if difference < -2*time.Millisecond || difference > 2*time.Millisecond {
					t.Fatalf("group uses replay time instead of execution evidence: title=%q attempt=%+v", group.title, attempt)
				}
			}
		}
	}
	markdown, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []string{"shared", "backend:build", "frontend:lint", "all"} {
		if !strings.Contains(string(markdown), task) {
			t.Fatalf("summary omitted %q: %s", task, markdown)
		}
	}
}

// Block inside replay, then require a sibling to finish emitting many more
// events than the engine subscriber capacity. Output speed cannot own workers.
type githubCLIBlockingWriter struct {
	bytes.Buffer
	blocked chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *githubCLIBlockingWriter) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("::group::first |")) {
		w.once.Do(func() { close(w.blocked); <-w.release })
	}
	return w.Buffer.Write(data)
}

func TestGitHubCISlowReplayDoesNotBlockSiblingExecution(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	out := &githubCLIBlockingWriter{blocked: make(chan struct{}), release: make(chan struct{})}
	secondDone := make(chan struct{})
	var emitted atomic.Int32
	p, root := githubCLIFixture(t, []project.Task{
		{Name: "first", Kind: project.KindOnce, Run: func(_ context.Context, rt *project.Runtime) error {
			rt.EmitLogLine("stdout", "first-retained-line")
			return nil
		}},
		{Name: "second", Kind: project.KindOnce, Run: func(ctx context.Context, rt *project.Runtime) error {
			select {
			case <-out.blocked:
			case <-ctx.Done():
				return ctx.Err()
			}
			// Subprocess readers retain one open handle and publish separately.
			// Test collector backpressure without 2048 unrelated file reopens.
			file, err := os.OpenFile(rt.LogPath, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return err
			}
			emit := rt.EventLineEmitter()
			for i := 0; i < 2048; i++ {
				line := fmt.Sprintf("second-retained-%04d", i)
				if _, err := fmt.Fprintln(file, "stdout: "+line); err != nil {
					_ = file.Close()
					return err
				}
				emit("stdout", line)
				emitted.Add(1)
			}
			if err := file.Close(); err != nil {
				return err
			}
			close(secondDone)
			return nil
		}},
	}, "first", "second")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := githubCLIRun(ctx, p, root, out, "--json", "--max-parallel", "2"); done <- err }()
	select {
	case <-secondDone:
	case <-ctx.Done():
		blocked := false
		select {
		case <-out.blocked:
			blocked = true
		default:
		}
		close(out.release)
		runErr := <-done
		t.Fatalf("sibling did not finish while output blocked: groupStarted=%t emitted=%d/2048 runErr=%v", blocked, emitted.Load(), runErr)
	}
	close(out.release)
	if err := <-done; err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	groups := githubCLIParseGroups(t, out.String())
	if len(groups) != 2 || strings.Count(out.String(), "second-retained-") != 2048 || strings.Count(out.String(), "first-retained-line") != 1 {
		t.Fatalf("grouped output lost or duplicated completed logs: groups=%d output bytes=%d", len(groups), out.Len())
	}
}

func TestGitHubCICacheFailureAndFinalSummary(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "LocalAppData"))
	summary := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)
	var executed atomic.Int32
	p, root := githubCLIFixture(t, []project.Task{
		{Name: "cached", Kind: project.KindOnce, Cache: true, Outputs: project.Outputs{Files: []string{"artifact.txt"}}, Run: func(_ context.Context, rt *project.Runtime) error {
			executed.Add(1)
			rt.EmitLogLine("stdout", "cached-body-marker")
			return os.WriteFile(rt.Abs("artifact.txt"), []byte("artifact"), 0o600)
		}},
		{Name: "failure", Kind: project.KindOnce, Deps: []string{"cached"}, Run: func(_ context.Context, rt *project.Runtime) error {
			rt.EmitLogLine("stderr", "failure-log-marker")
			return errors.New("intentional task failure")
		}},
		{Name: "blocked", Kind: project.KindOnce, Deps: []string{"failure"}, Run: func(context.Context, *project.Runtime) error { return errors.New("blocked dependency executed") }},
	}, "blocked")
	for i := 0; i < 2; i++ {
		var stderr bytes.Buffer
		result, stdout, err := githubCLIRun(context.Background(), p, root, &stderr, "--json")
		if err == nil || result.Success || result.FailedNode != "failure" {
			t.Fatalf("failure outcome changed: %v %s", err, stdout)
		}
		if count := strings.Count(stderr.String(), "::error title=failure::"); count != 1 {
			t.Fatalf("failure annotation count=%d:\n%s", count, &stderr)
		}
		if strings.Count(stderr.String(), "failure-log-marker") != 1 {
			t.Fatalf("failure output lost or duplicated:\n%s", &stderr)
		}
		groups := githubCLIParseGroups(t, stderr.String())
		if len(groups) != 2 {
			t.Fatalf("expected two attempts, blocked task must only enter summary: %+v", groups)
		}
		if i == 1 && (!strings.Contains(stderr.String(), "::group::cached | CACHED |") || strings.Contains(stderr.String(), "cached-body-marker")) {
			t.Fatalf("cache hit misrepresented execution:\n%s", &stderr)
		}
		if executed.Load() != 1 {
			t.Fatalf("cache presentation reran task: %d", executed.Load())
		}
		markdown, readErr := os.ReadFile(summary)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !strings.Contains(string(markdown), "failure") || !strings.Contains(string(markdown), "FAILED") || !strings.Contains(string(markdown), "blocked") || !strings.Contains(string(markdown), "BLOCKED") {
			t.Fatalf("summary lost failed or blocked evidence: %s", markdown)
		}
	}
}

type githubCLIService struct {
	stopped atomic.Bool
	runtime *project.Runtime
	done    chan struct{}
}

func (*githubCLIService) PID() int      { return 0 }
func (h *githubCLIService) Alive() bool { return !h.stopped.Load() }
func (h *githubCLIService) Wait() error { <-h.done; return nil }
func (h *githubCLIService) Stop() error {
	if !h.stopped.Swap(true) {
		h.runtime.EmitLogLine("stdout", "service-cleanup-output-marker")
		close(h.done)
	}
	return nil
}

func TestGitHubCIServiceCleanupAndInterruptedLogs(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted=%t", interrupted), func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", "true")
			t.Setenv("GITHUB_STEP_SUMMARY", "")
			handle := &githubCLIService{done: make(chan struct{})}
			started := make(chan struct{})
			p, root := githubCLIFixture(t, []project.Task{
				{Name: "service", Kind: project.KindService, Run: func(_ context.Context, rt *project.Runtime) error {
					handle.runtime = rt
					rt.RegisterServiceHandle(handle)
					rt.EmitLogLine("stdout", "service-running-output-marker")
					return nil
				}, Ready: func(context.Context, *project.Runtime) error { return nil }},
				{Name: "check", Kind: project.KindOnce, Deps: []string{"service"}, Run: func(ctx context.Context, rt *project.Runtime) error {
					rt.EmitLogLine("stdout", "check-available-output-marker")
					close(started)
					if interrupted {
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				}},
			}, "check")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stderr bytes.Buffer
			type completion struct {
				result api.RunResult
				stdout string
				err    error
			}
			done := make(chan completion, 1)
			go func() {
				result, stdout, err := githubCLIRun(ctx, p, root, &stderr, "--json")
				done <- completion{result, stdout, err}
			}()
			select {
			case <-started:
			case <-time.After(15 * time.Second):
				t.Fatal("service did not become ready")
			}
			if interrupted {
				cancel()
			}
			value := <-done
			if value.result.Success == interrupted || (value.err == nil) == interrupted || handle.Alive() {
				t.Fatalf("cleanup/outcome changed: alive=%t err=%v stdout=%s", handle.Alive(), value.err, value.stdout)
			}
			groups := githubCLIParseGroups(t, stderr.String())
			if len(groups) != 2 {
				t.Fatalf("ready service grouped early or final attempt omitted: %s", &stderr)
			}
			for _, marker := range []string{"service-running-output-marker", "service-cleanup-output-marker", "check-available-output-marker"} {
				if strings.Count(stderr.String(), marker) != 1 {
					t.Fatalf("available log omitted or repeated: %s", &stderr)
				}
			}
			for _, group := range groups {
				if strings.HasPrefix(group.title, "service |") && (!strings.Contains(group.title, "STOPPED") || !strings.Contains(group.body, "service-cleanup-output-marker")) {
					t.Fatalf("service group preceded owned cleanup: %+v", group)
				}
				if interrupted && strings.HasPrefix(group.title, "check |") && !strings.Contains(group.title, "CANCELED") {
					t.Fatalf("interrupted attempt mislabeled: %+v", group)
				}
			}
		})
	}
}

func TestBootstrapGitHubCIPresentationUsesInvocationEnvironment(t *testing.T) {
	isolateJSONContractState(t)
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(root, "examples", "github-actions", "devflow.project.go"))
	if err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	writeLocalProjectFile(t, worktree, string(source))
	for _, environment := range []string{"true", "false", "", "<unset>"} {
		for _, jsonOut := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", environment, jsonOut), func(t *testing.T) {
				t.Setenv("GITHUB_ACTIONS", environment)
				if environment == "<unset>" {
					if err := os.Unsetenv("GITHUB_ACTIONS"); err != nil {
						t.Fatal(err)
					}
				}
				summary := filepath.Join(t.TempDir(), "summary.md")
				t.Setenv("GITHUB_STEP_SUMMARY", summary)
				args := []string{"run", "verify", "--ci"}
				if jsonOut {
					args = append(args, "--json")
				}
				stdout, stderr, err := runJSONContractCommand(t, worktree, args...)
				if err != nil {
					t.Fatalf("compiled invocation: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
				}
				if jsonOut {
					var result api.RunResult
					if err := json.Unmarshal([]byte(stdout), &result); err != nil || !result.Success || result.Mode != api.ModeCI {
						t.Fatalf("bootstrap changed JSON/execution: %v %s", err, stdout)
					}
				}
				grouped := environment == "true"
				if strings.Contains(stderr, "::group::frontend:lint | SUCCESS |") != grouped {
					t.Fatalf("bootstrap lost invocation environment: %s", stderr)
				}
				if grouped {
					if strings.Count(stderr, "frontend-lint-line-1") != 1 {
						t.Fatalf("compiled task output lost or duplicated: %s", stderr)
					}
					if markdown, err := os.ReadFile(summary); err != nil || !bytes.Contains(markdown, []byte("frontend:lint")) {
						t.Fatalf("bootstrap did not inherit summary destination: err=%v summary=%s", err, markdown)
					}
				}
			})
		}
	}
	for _, progress := range []string{"states", "quiet"} {
		t.Run(progress, func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", "true")
			t.Setenv("GITHUB_STEP_SUMMARY", "")
			stdout, stderr, err := runJSONContractCommand(t, worktree, "run", "verify", "--ci", "--json", "--progress", progress)
			if err != nil || !json.Valid([]byte(stdout)) {
				t.Fatalf("compiled %s: %v stdout=%s stderr=%s", progress, err, stdout, stderr)
			}
			if strings.Contains(stderr, "::group::") || strings.Contains(stderr, "frontend-lint-line-1") || (progress == "quiet" && stderr != "") {
				t.Fatalf("compiled progress control changed: %s", stderr)
			}
			if progress == "states" && !strings.Contains(stderr, "[devflow]") {
				t.Fatalf("compiled states lost lifecycle: %s", stderr)
			}
		})
	}
}

func TestGitHubCISummaryFollowsRepositoryFinalization(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	summary := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)
	worktree := initRepositoryRepairGitWorktree(t)
	result, stdout, stderr, runErr := runRepositoryRepair(t, worktree, "repair-changes", "--fail-after-commit")
	if runErr == nil || result.Success || result.RepositoryChanges == nil || !result.RepositoryChanges.CommitCreated {
		t.Fatalf("fixture did not fail after successful task execution and repository commit: %v stdout=%s stderr=%s", runErr, stdout, stderr)
	}
	markdown, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(markdown), "FAILED") || !strings.Contains(string(markdown), "SUCCESS") {
		t.Fatalf("summary must distinguish successful tasks from failed enclosing finalization: %s", markdown)
	}
	if strings.Contains(stderr, "::error title=") {
		t.Fatalf("repository finalization mislabeled a successful task as failed: %s", stderr)
	}
}

func TestGitHubCIPresentationProblemsPreserveExecutionResult(t *testing.T) {
	for _, missingLog := range []bool{false, true} {
		t.Run(fmt.Sprintf("missingLog=%t", missingLog), func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", "true")
			if missingLog {
				t.Setenv("GITHUB_STEP_SUMMARY", "")
			} else {
				t.Setenv("GITHUB_STEP_SUMMARY", t.TempDir())
			}
			p, root := githubCLIFixture(t, []project.Task{{Name: "check", Kind: project.KindOnce, Run: func(_ context.Context, rt *project.Runtime) error {
				rt.EmitLogLine("stdout", "presentation-fixture-output")
				if missingLog {
					return os.Remove(rt.LogPath)
				}
				return nil
			}}}, "check")
			var stderr bytes.Buffer
			result, stdout, err := githubCLIRun(context.Background(), p, root, &stderr, "--json")
			if err != nil || !result.Success {
				t.Fatalf("presentation failure replaced execution outcome: %v stdout=%s stderr=%s", err, stdout, &stderr)
			}
			if !strings.Contains(strings.ToLower(stderr.String()), "presentation") {
				t.Fatalf("presentation failure had no diagnostic: %s", &stderr)
			}
			groups := githubCLIParseGroups(t, stderr.String())
			if len(groups) != 1 {
				t.Fatalf("presentation failure left malformed group: %s", &stderr)
			}
		})
	}
}

func TestGitHubCIPresentationIgnoresAdapterEnvironment(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	p, root := githubCLIFixture(t, []project.Task{{Name: "check", Kind: project.KindOnce, Run: func(_ context.Context, rt *project.Runtime) error {
		rt.EmitLogLine("stdout", "ordinary-output-marker")
		return nil
	}}}, "check")
	p.githubEnvironment = "true"
	var stderr bytes.Buffer
	result, stdout, err := githubCLIRun(context.Background(), p, root, &stderr, "--json")
	if err != nil || !result.Success || strings.Contains(stderr.String(), "::group::") || !strings.Contains(stderr.String(), "[devflow] check stdout: ordinary-output-marker") {
		t.Fatalf("adapter env changed invocation presentation: %v stdout=%s stderr=%s", err, stdout, &stderr)
	}
}

func TestGitHubEnvironmentDoesNotSelectCIMode(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	p, root := githubCLIFixture(t, []project.Task{{Name: "check", Kind: project.KindOnce, Run: func(_ context.Context, rt *project.Runtime) error {
		if rt.Mode != api.ModeDev {
			return fmt.Errorf("environment changed mode to %s", rt.Mode)
		}
		rt.EmitLogLine("stdout", "ordinary-development-output")
		return nil
	}}}, "check")
	t.Cleanup(func() {
		client, err := daemon.Dial(root)
		if err != nil {
			t.Error(err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := client.Call(ctx, daemon.Request{Action: daemon.ActionStop, All: true}); err != nil {
			t.Error(err)
		}
	})
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run([]string{"run", "verify", "--project", p.Name(), "--worktree", root, "--json"})
	var result api.RunResult
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); err != nil || decodeErr != nil || !result.Success || result.Mode != api.ModeDev {
		t.Fatalf("GitHub selected CI execution: err=%v decode=%v stdout=%s stderr=%s", err, decodeErr, &stdout, &stderr)
	}
	if strings.Contains(stderr.String(), "::group::") {
		t.Fatalf("finite CI presentation entered development mode: %s", &stderr)
	}
}

func TestGitHubCIRetainedLogJSONLRemainsRaw(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	const childMarker = "::group::raw-retained-child-marker"
	p, root := githubCLIFixture(t, []project.Task{{Name: "check", Kind: project.KindOnce, Run: func(_ context.Context, rt *project.Runtime) error {
		rt.EmitLogLine("stdout", childMarker)
		rt.EmitLogLine("stdout", "::endgroup::")
		return nil
	}}}, "check")
	var stderr bytes.Buffer
	result, stdout, err := githubCLIRun(context.Background(), p, root, &stderr, "--json")
	if err != nil {
		t.Fatalf("run: %v stdout=%s stderr=%s", err, stdout, &stderr)
	}
	var logOutput, logError bytes.Buffer
	app := &App{Stdout: &logOutput, Stderr: &logError}
	if err := app.Run([]string{"logs", "check", "--run", result.RunID, "--worktree", root, "--json"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(logOutput.String(), "\n"), "\n")
	if len(lines) != 2 || logError.Len() != 0 {
		t.Fatalf("GitHub presentation changed log JSONL shape: stdout=%s stderr=%s", &logOutput, &logError)
	}
	for i, line := range lines {
		var entry map[string]string
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["runId"] != result.RunID || entry["attemptId"] != result.Nodes[0].AttemptID || entry["task"] != "check" || (i == 0 && entry["line"] != "stdout: "+childMarker) {
			t.Fatalf("presentation altered raw retained log contract: %s", line)
		}
	}
}

func TestCompiledGitHubWatchPreservesJSONLContract(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	// Reuse the transport contract's start/event/error assertions under the
	// hosted invocation environment, including the project-local bootstrap.
	TestCompiledWatchJSONIsJSONLThroughTransportFailure(t)
}
