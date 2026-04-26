package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	embeddedwebapp "github.com/benjaco/devflow/examples/embedded-web-app"
	gonextmonorepo "github.com/benjaco/devflow/examples/go-next-monorepo"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/cache"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

type failCLIProject struct{}
type taskTargetCLIProject struct{}
type depsCLIProject struct{}

func (failCLIProject) Name() string { return "cli-fail-project" }

func (failCLIProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "cli-fail"}, nil
}

func (failCLIProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name: "fail",
			Kind: project.KindOnce,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				_ = rt
				return fmt.Errorf("boom")
			},
		},
	}
}

func (failCLIProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"fail"}}}
}

func (taskTargetCLIProject) Name() string { return "cli-task-target-project" }

func (taskTargetCLIProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "cli-task-target"}, nil
}

func (taskTargetCLIProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:    "gen",
			Kind:    project.KindOnce,
			Cache:   true,
			Outputs: project.Outputs{Files: []string{"gen.txt"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				return os.WriteFile(filepath.Join(rt.Worktree, "gen.txt"), []byte("ok"), 0o644)
			},
		},
	}
}

func (taskTargetCLIProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"gen"}}}
}

func (depsCLIProject) Name() string { return "cli-deps-project" }

func (depsCLIProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "cli-deps"}, nil
}

func (depsCLIProject) Tasks() []project.Task { return nil }

func (depsCLIProject) Targets() []project.Target {
	return []project.Target{{Name: "noop", RootTasks: nil}}
}

func (depsCLIProject) Dependencies() []project.Dependency {
	marker := filepath.Join(os.TempDir(), "devflow-cli-deps-installed.txt")
	bin := filepath.Join(os.TempDir(), "devflow-cli-missing-tool")
	installer := strings.Join([]string{
		"echo installed > " + shellQuote(marker),
		"cat > " + shellQuote(bin) + " <<'EOF'",
		"#!/bin/sh",
		"exit 0",
		"EOF",
		"chmod +x " + shellQuote(bin),
	}, "\n")
	return []project.Dependency{
		{Name: "shell", Command: "sh"},
		{
			Name:    "missing-tool",
			Command: "devflow-cli-missing-tool",
			Install: map[string]project.InstallScript{runtime.GOOS: {Script: installer}},
		},
	}
}

func init() {
	project.Register(failCLIProject{})
	project.Register(taskTargetCLIProject{})
	project.Register(depsCLIProject{})
}

func TestGraphListJSON(t *testing.T) {
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"graph", "list", "--json", "--project", "go-next-monorepo"}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(app.Stdout.(*bytes.Buffer).Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["tasks"]; !ok {
		t.Fatalf("missing tasks: %v", payload)
	}
}

func TestVersionJSON(t *testing.T) {
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"version", "--json"}); err != nil {
		t.Fatal(err)
	}
	var result api.VersionResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ModulePath != "github.com/benjaco/devflow" {
		t.Fatalf("unexpected module path %q", result.ModulePath)
	}
	if result.Version == "" || result.GoVersion == "" {
		t.Fatalf("expected version and go version, got %+v", result)
	}
}

func TestUpgradeJSONRunsGoInstallLatest(t *testing.T) {
	argsPath := installFakeGo(t, 0)
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"upgrade", "--json"}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "install github.com/benjaco/devflow/cmd/devflow@latest\n"
	if string(args) != wantArgs {
		t.Fatalf("unexpected go args: got %q want %q", string(args), wantArgs)
	}
	var result api.UpgradeResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.VersionTarget != "latest" {
		t.Fatalf("unexpected upgrade result: %+v", result)
	}
}

func TestUpgradeVersionJSON(t *testing.T) {
	argsPath := installFakeGo(t, 0)
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"upgrade", "--json", "--version", "v0.1.2"}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "install github.com/benjaco/devflow/cmd/devflow@v0.1.2\n"
	if string(args) != wantArgs {
		t.Fatalf("unexpected go args: got %q want %q", string(args), wantArgs)
	}
}

func TestUpgradeJSONReportsFailure(t *testing.T) {
	installFakeGo(t, 7)
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	err := app.Run([]string{"upgrade", "--json"})
	if err == nil {
		t.Fatal("expected upgrade failure")
	}
	var result api.UpgradeResult
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if result.Success || result.Error == "" {
		t.Fatalf("expected structured failure, got %+v", result)
	}
}

func TestRunJSONStillReturnsExecutionError(t *testing.T) {
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	err := app.Run([]string{"run", "build", "--json", "--ci", "--project", "cli-fail-project", "--worktree", t.TempDir()})
	if err == nil {
		t.Fatal("expected run command to return task failure even with --json")
	}
}

func TestRunAcceptsTaskNameAsSyntheticTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	worktree := t.TempDir()
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"run", "gen", "--json", "--ci", "--project", "cli-task-target-project", "--worktree", worktree}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "gen.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ok" {
		t.Fatalf("unexpected generated data %q", string(data))
	}
}

func TestFlushAutoStartsDetachedWatchJSON(t *testing.T) {
	worktree := t.TempDir()
	writeLocalProjectFile(t, worktree, localProjectSource("local-flush-project", "up"))
	t.Cleanup(func() {
		_, _ = runBootstrapCommand(t, worktree, "stop", "--all", "--json", "--worktree", worktree)
	})

	output, err := runBootstrapCommand(t, worktree, "flush", "up", "--json", "--timeout", "10s", "--worktree", worktree)
	if err != nil {
		t.Fatalf("flush failed: %v\n%s", err, output)
	}
	var result api.FlushResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode flush result %q: %v", output, err)
	}
	if !result.Success || !result.Synced || !result.Started {
		t.Fatalf("expected successful auto-started flush, got %+v", result)
	}
	if result.Target != "up" {
		t.Fatalf("unexpected target %q", result.Target)
	}
}

func TestFlushNoTargetUsesPreferredTarget(t *testing.T) {
	worktree := t.TempDir()
	writeLocalProjectFile(t, worktree, localProjectSource("local-flush-default-project", "up"))
	t.Cleanup(func() {
		_, _ = runBootstrapCommand(t, worktree, "stop", "--all", "--json", "--worktree", worktree)
	})

	output, err := runBootstrapCommand(t, worktree, "flush", "--json", "--timeout", "10s", "--worktree", worktree)
	if err != nil {
		t.Fatalf("flush failed: %v\n%s", err, output)
	}
	result := decodeCLIFlushResult(t, []byte(output))
	if !result.Success || result.Target != "up" || !result.Started {
		t.Fatalf("expected preferred target auto-start flush, got %+v", result)
	}
}

func TestFlushNoTargetUsesLastRunTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	recordCLITestSupervisor(t, worktree, api.RunConfig{
		Project:  "cli-task-target-project",
		Target:   "build",
		Mode:     api.ModeWatch,
		Detached: true,
	})

	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	err := app.Run([]string{"flush", "--json", "--worktree", worktree, "--timeout", "10ms"})
	if err == nil {
		t.Fatal("expected flush to time out without a real watcher")
	}
	var result api.FlushResult
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatalf("decode flush result: %v\n%s", decodeErr, stdout.String())
	}
	if result.Target != "build" || !result.TimedOut {
		t.Fatalf("expected last-run target timeout, got %+v", result)
	}
}

func TestFlushLiveWatchTargetMismatchFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	recordCLITestSupervisor(t, worktree, api.RunConfig{
		Project:  "cli-task-target-project",
		Target:   "gen",
		Mode:     api.ModeWatch,
		Detached: true,
	})

	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	err := app.Run([]string{"flush", "build", "--json", "--project", "cli-task-target-project", "--worktree", worktree})
	if err == nil {
		t.Fatal("expected target mismatch error")
	}
	result := decodeCLIFlushResult(t, stdout.Bytes())
	if len(result.Issues) != 1 || result.Issues[0].Kind != "target_mismatch" {
		t.Fatalf("unexpected mismatch result: %+v", result)
	}
}

func TestFlushLiveNonWatchSupervisorFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	recordCLITestSupervisor(t, worktree, api.RunConfig{
		Project:  "cli-task-target-project",
		Target:   "build",
		Mode:     api.ModeDev,
		Detached: true,
	})

	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	err := app.Run([]string{"flush", "build", "--json", "--project", "cli-task-target-project", "--worktree", worktree})
	if err == nil {
		t.Fatal("expected non-watch supervisor error")
	}
	result := decodeCLIFlushResult(t, stdout.Bytes())
	if len(result.Issues) != 1 || result.Issues[0].Kind != "non_watch_supervisor" {
		t.Fatalf("unexpected non-watch result: %+v", result)
	}
}

func TestFlushTimeoutReturnsStructuredFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	recordCLITestSupervisor(t, worktree, api.RunConfig{
		Project:  "cli-task-target-project",
		Target:   "build",
		Mode:     api.ModeWatch,
		Detached: true,
	})

	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	err := app.Run([]string{"flush", "build", "--json", "--project", "cli-task-target-project", "--worktree", worktree, "--timeout", "10ms"})
	if err == nil {
		t.Fatal("expected flush timeout")
	}
	result := decodeCLIFlushResult(t, stdout.Bytes())
	if !result.TimedOut || result.Success || len(result.Issues) != 1 || result.Issues[0].Kind != "timeout" {
		t.Fatalf("unexpected timeout result: %+v", result)
	}
}

func TestDepsStatusAndInstallJSON(t *testing.T) {
	marker := filepath.Join(os.TempDir(), "devflow-cli-deps-installed.txt")
	_ = os.Remove(marker)
	bin := filepath.Join(os.TempDir(), "devflow-cli-missing-tool")
	_ = os.Remove(bin)
	t.Setenv("PATH", os.TempDir()+string(os.PathListSeparator)+os.Getenv("PATH"))

	statusOut := &bytes.Buffer{}
	app := &App{Stdout: statusOut, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"deps", "status", "--json", "--project", "cli-deps-project", "--worktree", t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	var statusPayload map[string]any
	if err := json.Unmarshal(statusOut.Bytes(), &statusPayload); err != nil {
		t.Fatal(err)
	}
	deps, ok := statusPayload["dependencies"].([]any)
	if !ok || len(deps) != 2 {
		t.Fatalf("unexpected deps payload: %+v", statusPayload)
	}

	installOut := &bytes.Buffer{}
	app = &App{Stdout: installOut, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"deps", "install", "--json", "--project", "cli-deps-project", "--worktree", t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected install marker to be written: %v", err)
	}
}

func shellQuote(value string) string {
	return "'" + value + "'"
}

func TestDefaultLaunchPlanStartsDetachedForFreshDetectedWorktree(t *testing.T) {
	worktree := t.TempDir()
	if err := gonextmonorepo.SeedWorktree(worktree); err != nil {
		t.Fatal(err)
	}
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	plan, err := app.defaultLaunchPlan(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if plan.projectName != "go-next-monorepo" {
		t.Fatalf("unexpected project %q", plan.projectName)
	}
	if plan.target != "fullstack" {
		t.Fatalf("unexpected target %q", plan.target)
	}
	if !plan.startDetached {
		t.Fatal("expected fresh worktree to start detached")
	}
}

func TestDefaultLaunchPlanAttachesToExistingDetachedSupervisor(t *testing.T) {
	worktree := t.TempDir()
	if err := embeddedwebapp.SeedWorktree(worktree); err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Resolve(worktree, filepath.Base(worktree))
	if err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	if err := instance.RecordDetachedRun(inst, api.RunConfig{
		Project:  "embedded-web-app",
		Target:   "up",
		Mode:     api.ModeDev,
		Detached: true,
	}, pid, filepath.Join(worktree, ".devflow", "logs", inst.ID, "supervisor.log")); err != nil {
		t.Fatal(err)
	}
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	plan, err := app.defaultLaunchPlan(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if plan.projectName != "embedded-web-app" {
		t.Fatalf("unexpected project %q", plan.projectName)
	}
	if plan.target != "up" {
		t.Fatalf("unexpected target %q", plan.target)
	}
	if plan.startDetached {
		t.Fatal("expected existing live supervisor to reuse current instance")
	}
}

var (
	bootstrapBuildOnce sync.Once
	bootstrapBinary    string
	bootstrapBuildErr  error
)

func TestBootstrapExecsLocalProjectBinary(t *testing.T) {
	worktree := t.TempDir()
	writeLocalProjectFile(t, worktree, localProjectSource("local-bootstrap-project", "up"))

	output, err := runBootstrapCommand(t, worktree, "graph", "list", "--json")
	if err != nil {
		t.Fatalf("bootstrap command failed: %v\n%s", err, output)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		t.Fatalf("failed to decode output %q: %v", output, err)
	}
	targets, ok := payload["targets"].([]any)
	if !ok || len(targets) == 0 {
		t.Fatalf("unexpected targets payload: %+v", payload)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".devflow", "bin", "devflow-local")); err != nil {
		t.Fatalf("expected local binary to be built: %v", err)
	}
}

func TestBootstrapWritesWorktreeLocalBuildModuleWithSourceReplace(t *testing.T) {
	worktree := t.TempDir()
	writeLocalProjectFile(t, worktree, localProjectSource("local-build-module-project", "up"))

	output, err := runBootstrapCommand(t, worktree, "graph", "list", "--json")
	if err != nil {
		t.Fatalf("bootstrap command failed: %v\n%s", err, output)
	}
	buildRoot := filepath.Join(worktree, ".devflow", "localbuild")
	entries, err := os.ReadDir(buildRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one local build dir, got %d", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(buildRoot, entries[0].Name(), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "module github.com/benjaco/devflow/localbuild/") {
		t.Fatalf("expected generated module path, got:\n%s", text)
	}
	repoRoot, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "replace github.com/benjaco/devflow => "+filepath.ToSlash(repoRoot)) {
		t.Fatalf("expected source replace in generated go.mod, got:\n%s", text)
	}
}

func TestLocalBuildModuleSourceInstalledModeUsesVersionWithoutReplace(t *testing.T) {
	t.Setenv(envBootstrapModuleVersion, "v1.2.3")
	buildDir := filepath.Join(t.TempDir(), ".devflow", "localbuild", "abc123")
	data, err := localBuildModuleSource(buildDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, "require github.com/benjaco/devflow v1.2.3") {
		t.Fatalf("expected requested module version, got:\n%s", data)
	}
	if strings.Contains(data, "replace github.com/benjaco/devflow") {
		t.Fatalf("installed-mode module should not include source replace, got:\n%s", data)
	}
}

func TestBootstrapFailsWithoutLocalProjectFile(t *testing.T) {
	worktree := t.TempDir()
	output, err := runBootstrapCommand(t, worktree, "graph", "list", "--json")
	if err == nil {
		t.Fatalf("expected bootstrap command to fail without local project file, got output %q", output)
	}
	if !strings.Contains(output, "devflow.project.go not found") {
		t.Fatalf("unexpected error output: %q", output)
	}
}

func TestBootstrapRebuildsWhenLocalProjectChanges(t *testing.T) {
	worktree := t.TempDir()
	projectPath := filepath.Join(worktree, localProjectFile)
	writeLocalProjectFile(t, worktree, localProjectSource("local-rebuild-project", "up"))

	if _, err := runBootstrapCommand(t, worktree, "graph", "list", "--json"); err != nil {
		t.Fatalf("initial bootstrap command failed: %v", err)
	}
	binaryPath := filepath.Join(worktree, ".devflow", "bin", "devflow-local")
	before, err := os.Stat(binaryPath)
	if err != nil {
		t.Fatal(err)
	}

	updated := localProjectSource("local-rebuild-project", "build")
	if err := os.WriteFile(projectPath, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	future := before.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(projectPath, future, future); err != nil {
		t.Fatal(err)
	}

	output, err := runBootstrapCommand(t, worktree, "graph", "list", "--json")
	if err != nil {
		t.Fatalf("rebuild bootstrap command failed: %v\n%s", err, output)
	}
	after, err := os.Stat(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().After(before.ModTime()) {
		t.Fatalf("expected local binary modtime to increase: before=%s after=%s", before.ModTime(), after.ModTime())
	}
	if !strings.Contains(output, "\"build\"") {
		t.Fatalf("expected updated target in output, got %q", output)
	}
}

func TestBootstrapDoesNotRebuildOnTimestampOnlyChange(t *testing.T) {
	worktree := t.TempDir()
	projectPath := filepath.Join(worktree, localProjectFile)
	writeLocalProjectFile(t, worktree, localProjectSource("local-stable-project", "up"))

	if _, err := runBootstrapCommand(t, worktree, "graph", "list", "--json"); err != nil {
		t.Fatalf("initial bootstrap command failed: %v", err)
	}
	binaryPath := filepath.Join(worktree, ".devflow", "bin", "devflow-local")
	before, err := os.Stat(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := localBuildKeyPath(binaryPath)
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("expected build key file to exist: %v", err)
	}

	future := before.ModTime().Add(3 * time.Second)
	if err := os.Chtimes(projectPath, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := runBootstrapCommand(t, worktree, "graph", "list", "--json"); err != nil {
		t.Fatalf("timestamp-only bootstrap command failed: %v", err)
	}
	after, err := os.Stat(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("expected binary modtime to stay unchanged on timestamp-only touch: before=%s after=%s", before.ModTime(), after.ModTime())
	}
}

func TestBootstrapFailedRebuildKeepsPreviousBinary(t *testing.T) {
	worktree := t.TempDir()
	projectPath := filepath.Join(worktree, localProjectFile)
	projectName := "local-atomic-project"
	writeLocalProjectFile(t, worktree, localProjectSource(projectName, "up"))

	if _, err := runBootstrapCommand(t, worktree, "graph", "list", "--json"); err != nil {
		t.Fatalf("initial bootstrap command failed: %v", err)
	}
	binaryPath := filepath.Join(worktree, ".devflow", "bin", "devflow-local")
	before, err := os.Stat(binaryPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(projectPath, []byte("package main\n\nfunc broken(\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := runBootstrapCommand(t, worktree, "graph", "list", "--json")
	if err == nil {
		t.Fatalf("expected rebuild to fail for invalid local project, got output %q", output)
	}

	after, err := os.Stat(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("expected existing binary to stay in place after failed rebuild: before=%s after=%s", before.ModTime(), after.ModTime())
	}

	cmd := exec.Command(binaryPath, "graph", "list", "--json", "--project", projectName)
	cmd.Dir = worktree
	cmd.Env = withEnv(os.Environ(), envLocalExec, "1")
	directOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected previous local binary to remain runnable: %v\n%s", err, string(directOut))
	}
	if !strings.Contains(string(directOut), "\"up\"") {
		t.Fatalf("expected previous binary output to include old target, got %q", string(directOut))
	}
}

func buildBootstrapBinary(t *testing.T) string {
	t.Helper()
	bootstrapBuildOnce.Do(func() {
		repoRoot, err := repoRoot()
		if err != nil {
			bootstrapBuildErr = err
			return
		}
		dir, err := os.MkdirTemp("", "devflow-bootstrap-bin-*")
		if err != nil {
			bootstrapBuildErr = err
			return
		}
		path := filepath.Join(dir, "devflow-test-bootstrap")
		cmd := exec.Command("go", "build", "-o", path, "./cmd/devflow")
		cmd.Dir = repoRoot
		output, err := cmd.CombinedOutput()
		if err != nil {
			bootstrapBuildErr = fmt.Errorf("bootstrap build failed: %w\n%s", err, strings.TrimSpace(string(output)))
			return
		}
		bootstrapBinary = path
	})
	if bootstrapBuildErr != nil {
		t.Fatal(bootstrapBuildErr)
	}
	return bootstrapBinary
}

func runBootstrapCommand(t *testing.T, worktree string, args ...string) (string, error) {
	t.Helper()
	repoRoot, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(buildBootstrapBinary(t), args...)
	cmd.Dir = worktree
	cmd.Env = withEnv(os.Environ(), envBootstrapEntry, "1")
	cmd.Env = withEnv(cmd.Env, envBootstrapRoot, repoRoot)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(wd, "..", ".."))
}

func writeLocalProjectFile(t *testing.T, worktree, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(worktree, localProjectFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func installFakeGo(t *testing.T, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	fakeGo := filepath.Join(dir, "go")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" > "$DEVFLOW_FAKE_GO_ARGS"
echo fake go output
exit %d
`, exitCode)
	if err := os.WriteFile(fakeGo, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEVFLOW_FAKE_GO_ARGS", argsPath)
	return argsPath
}

func localProjectSource(name, target string) string {
	return fmt.Sprintf(`package main

import (
	"context"

	"github.com/benjaco/devflow/pkg/project"
)

type localProject struct{}

func init() {
	project.Register(localProject{})
}

func (localProject) Name() string { return %q }

func (localProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "local"}, nil
}

func (localProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name: "noop",
			Kind: project.KindOnce,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				_ = rt
				return nil
			},
		},
	}
}

func (localProject) Targets() []project.Target {
	return []project.Target{{Name: %q, RootTasks: []string{"noop"}}}
}
`, name, target)
}

func TestStartEventCapturePersistsJSONLines(t *testing.T) {
	worktree := t.TempDir()
	instanceID, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan api.Event, 4)
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	stop, err := app.startEventCapture(worktree, instanceID, events)
	if err != nil {
		t.Fatal(err)
	}
	events <- api.Event{Type: api.EventRunStarted, InstanceID: instanceID, Target: "fullstack"}
	events <- api.Event{Type: api.EventWatchCycleStart, InstanceID: instanceID, Files: []string{"frontend/src/page.tsx"}, AffectedTasks: []string{"frontend_dev"}}
	stop()

	data, err := os.ReadFile(instance.EventsPath(worktree, instanceID))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected 2 persisted event lines, got %d", len(lines))
	}
	var payload map[string]any
	if err := json.Unmarshal(lines[1], &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload["type"]; got != string(api.EventWatchCycleStart) {
		t.Fatalf("unexpected event type %v", got)
	}
	affected, ok := payload["affectedTasks"].([]any)
	if !ok || len(affected) != 1 || affected[0] != "frontend_dev" {
		t.Fatalf("unexpected affectedTasks payload: %v", payload["affectedTasks"])
	}
}

func TestCacheStatusJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	worktree := t.TempDir()
	store := cache.NewNamespaced(instance.CacheRoot(), project.CacheNamespace(taskTargetCLIProject{}))
	if err := os.WriteFile(filepath.Join(worktree, "out.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Snapshot(worktree, project.Task{
		Name:    "gen",
		Kind:    project.KindOnce,
		Outputs: project.Outputs{Files: []string{"out.txt"}},
	}, "key1"); err != nil {
		t.Fatal(err)
	}
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"cache", "status", "--json", "--project", "cli-task-target-project", "--worktree", worktree}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got := int(payload["count"].(float64)); got != 1 {
		t.Fatalf("unexpected cache count: %d", got)
	}
	if got := payload["namespace"]; got != "cli-task-target-project" {
		t.Fatalf("unexpected cache namespace: %v", got)
	}
}

func TestStopCommandStopsTrackedProcess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle, err := process.Start(ctx, process.CommandSpec{
		Name: "sh",
		Args: []string{"-c", "trap 'exit 0' INT TERM; while true; do sleep 1; done"},
		Dir:  worktree,
	})
	if err != nil {
		t.Fatal(err)
	}
	inst.Processes["svc"] = api.ProcessRef{PID: handle.PID(), StartedAt: time.Now().UTC()}
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}
	if err := instance.SaveStatus(worktree, inst.ID, "dev", api.ModeDev, map[string]api.NodeStatus{
		"svc": {Name: "svc", Kind: "service", State: api.StateRunning, PID: handle.PID()},
	}); err != nil {
		t.Fatal(err)
	}
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"stop", "--worktree", worktree, "--task", "svc"}); err != nil {
		t.Fatal(err)
	}
	waitForProcessExit(t, handle)
	state, err := instance.LoadStatus(worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Nodes["svc"].State; got != api.StateStopped {
		t.Fatalf("expected stopped state, got %s", got)
	}
}

func TestExampleProjectCLIJSONLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DEVFLOW_EXAMPLE_FAKE_DB", "1")
	worktree := t.TempDir()
	if err := seedExampleWorktree(worktree); err != nil {
		t.Fatal(err)
	}

	runStdout := &bytes.Buffer{}
	app := &App{Stdout: runStdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{
		"run", "fullstack",
		"--json",
		"--ci",
		"--project", "go-next-monorepo",
		"--worktree", worktree,
		"--max-parallel", "4",
	}); err != nil {
		t.Fatal(err)
	}

	var runResult api.RunResult
	if err := json.Unmarshal(runStdout.Bytes(), &runResult); err != nil {
		t.Fatal(err)
	}
	if !runResult.Success {
		t.Fatalf("expected successful run result: %+v", runResult)
	}
	if runResult.InstanceID == "" {
		t.Fatalf("expected instance ID in run result: %+v", runResult)
	}
	t.Cleanup(func() {
		inst, err := instance.Load(worktree, runResult.InstanceID)
		if err != nil {
			return
		}
		_, _ = instance.StopProcesses(inst, "")
	})
	runtimeEnvPath := filepath.Join(worktree, ".devflow", "state", "instances", runResult.InstanceID, "runtime.env")
	runtimeEnv, err := os.ReadFile(runtimeEnvPath)
	if err != nil {
		t.Fatal(err)
	}
	runtimeEnvText := string(runtimeEnv)
	if !strings.Contains(runtimeEnvText, "EXAMPLE_SHARED_FLAG=from-dotenv\n") {
		t.Fatalf("expected dotenv value in runtime env: %q", runtimeEnvText)
	}
	if strings.Contains(runtimeEnvText, "PGPORT=9999\n") {
		t.Fatalf("expected devflow-managed PGPORT override in runtime env: %q", runtimeEnvText)
	}
	if !strings.Contains(runtimeEnvText, "NEXTAUTH_URL=http://devflow.local.test\n") {
		t.Fatalf("expected NEXTAUTH_URL from dotenv in runtime env: %q", runtimeEnvText)
	}

	statusStdout := &bytes.Buffer{}
	app = &App{Stdout: statusStdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"status", "--json", "--worktree", worktree}); err != nil {
		t.Fatal(err)
	}
	var status api.StatusResult
	if err := json.Unmarshal(statusStdout.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.InstanceID != runResult.InstanceID {
		t.Fatalf("unexpected status instance ID: got %q want %q", status.InstanceID, runResult.InstanceID)
	}
	realWorktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		realWorktree = worktree
	}
	if status.Worktree != realWorktree {
		t.Fatalf("unexpected status worktree: got %q want %q", status.Worktree, realWorktree)
	}
	if status.URLs["backend"] == "" || status.URLs["frontend"] == "" {
		t.Fatalf("expected status URLs to be populated: %+v", status.URLs)
	}
	if status.DB.Password != "" || status.DB.URL != "" {
		t.Fatalf("expected status DB details to be sanitized: %+v", status.DB)
	}
	if len(status.Nodes) == 0 {
		t.Fatal("expected status nodes")
	}
	if !hasNodeState(status.Nodes, "backend_dev", api.StateRunning) {
		t.Fatalf("expected backend_dev running in status: %+v", status.Nodes)
	}
	if !hasNodeState(status.Nodes, "frontend_dev", api.StateRunning) {
		t.Fatalf("expected frontend_dev running in status: %+v", status.Nodes)
	}

	waitForCondition(t, 3*time.Second, func() bool {
		lines, err := readLastLines(instance.LogPath(worktree, runResult.InstanceID, "backend_dev"), 5)
		return err == nil && len(lines) > 0
	})

	logsStdout := &bytes.Buffer{}
	app = &App{Stdout: logsStdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"logs", "backend_dev", "--json", "--worktree", worktree, "--tail", "5"}); err != nil {
		t.Fatal(err)
	}
	logEvents := decodeJSONLines(t, logsStdout.Bytes())
	if len(logEvents) == 0 {
		t.Fatal("expected log events from logs command")
	}
	if got := logEvents[0]["task"]; got != "backend_dev" {
		t.Fatalf("unexpected logs task: %v", got)
	}
	if _, ok := logEvents[0]["line"]; !ok {
		t.Fatalf("expected log line payload: %v", logEvents[0])
	}
	if !strings.Contains(logEvents[0]["line"], "backend-dotenv") {
		t.Fatalf("expected backend log line to include dotenv flag, got %q", logEvents[0]["line"])
	}

	frontendLogsStdout := &bytes.Buffer{}
	app = &App{Stdout: frontendLogsStdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"logs", "frontend_dev", "--json", "--worktree", worktree, "--tail", "5"}); err != nil {
		t.Fatal(err)
	}
	frontendLogEvents := decodeJSONLines(t, frontendLogsStdout.Bytes())
	if len(frontendLogEvents) == 0 {
		t.Fatal("expected frontend log events from logs command")
	}
	if !strings.Contains(frontendLogEvents[0]["line"], "frontend-dotenv") {
		t.Fatalf("expected frontend log line to include dotenv flag, got %q", frontendLogEvents[0]["line"])
	}
	if !strings.Contains(frontendLogEvents[0]["line"], "http://devflow.local.test") {
		t.Fatalf("expected frontend log line to include NEXTAUTH_URL from dotenv, got %q", frontendLogEvents[0]["line"])
	}

	inst, err := instance.Load(worktree, runResult.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	supervisorCtx, supervisorCancel := context.WithCancel(context.Background())
	defer supervisorCancel()
	supervisorHandle, err := process.Start(supervisorCtx, process.CommandSpec{
		Name: "sh",
		Args: []string{"-c", "trap 'exit 0' INT TERM; while true; do sleep 1; done"},
		Dir:  worktree,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		supervisorCancel()
		_ = supervisorHandle.Wait()
	}()
	if err := instance.RecordDetachedRun(inst, api.RunConfig{
		Project:  "go-next-monorepo",
		Target:   "fullstack",
		Mode:     api.ModeDev,
		Detached: true,
	}, supervisorHandle.PID(), filepath.Join(worktree, ".devflow", "logs", runResult.InstanceID, "supervisor.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".devflow", "logs", runResult.InstanceID, "supervisor.log"), []byte("supervisor line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	statusStdout = &bytes.Buffer{}
	app = &App{Stdout: statusStdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"status", "--json", "--worktree", worktree}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(statusStdout.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Supervisor == nil || !status.Supervisor.Alive {
		t.Fatalf("expected live supervisor in status: %+v", status.Supervisor)
	}

	supervisorLogsStdout := &bytes.Buffer{}
	app = &App{Stdout: supervisorLogsStdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"logs", "supervisor", "--json", "--worktree", worktree, "--tail", "5"}); err != nil {
		t.Fatal(err)
	}
	supervisorLogEvents := decodeJSONLines(t, supervisorLogsStdout.Bytes())
	if len(supervisorLogEvents) == 0 {
		t.Fatal("expected supervisor log events from logs command")
	}
	if got := supervisorLogEvents[0]["task"]; got != "supervisor" {
		t.Fatalf("unexpected supervisor logs task: %v", got)
	}
	if supervisorLogEvents[0]["line"] != "supervisor line" {
		t.Fatalf("unexpected supervisor log line: %q", supervisorLogEvents[0]["line"])
	}

	instancesStdout := &bytes.Buffer{}
	app = &App{Stdout: instancesStdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"instances", "--json"}); err != nil {
		t.Fatal(err)
	}
	var instancesList []api.InstanceSummary
	if err := json.Unmarshal(instancesStdout.Bytes(), &instancesList); err != nil {
		t.Fatal(err)
	}
	if !containsInstance(instancesList, runResult.InstanceID) {
		t.Fatalf("expected instances list to contain %q: %+v", runResult.InstanceID, instancesList)
	}
	for _, item := range instancesList {
		if item.ID == runResult.InstanceID && (item.DB.Password != "" || item.DB.URL != "") {
			t.Fatalf("expected instance DB details to be sanitized: %+v", item.DB)
		}
	}

	doctorStdout := &bytes.Buffer{}
	app = &App{Stdout: doctorStdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"doctor", "--json", "--worktree", worktree, "--project", "go-next-monorepo"}); err != nil {
		t.Fatal(err)
	}
	var doctor api.DoctorResult
	if err := json.Unmarshal(doctorStdout.Bytes(), &doctor); err != nil {
		t.Fatal(err)
	}
	if !doctor.ChecksPassed {
		t.Fatalf("expected doctor checks to pass: %+v", doctor)
	}
	if doctor.InstanceID != runResult.InstanceID {
		t.Fatalf("unexpected doctor instance ID: got %q want %q", doctor.InstanceID, runResult.InstanceID)
	}

	stopStdout := &bytes.Buffer{}
	app = &App{Stdout: stopStdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"stop", "--json", "--worktree", worktree, "--all"}); err != nil {
		t.Fatal(err)
	}
	var stopPayload map[string]any
	if err := json.Unmarshal(stopStdout.Bytes(), &stopPayload); err != nil {
		t.Fatal(err)
	}
	stopped, ok := stopPayload["stopped"].([]any)
	if !ok || len(stopped) != 1 || stopped[0] != "supervisor" {
		t.Fatalf("expected stopped service list in stop payload: %v", stopPayload)
	}

	finalStatusStdout := &bytes.Buffer{}
	app = &App{Stdout: finalStatusStdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"status", "--json", "--worktree", worktree}); err != nil {
		t.Fatal(err)
	}
	var finalStatus api.StatusResult
	if err := json.Unmarshal(finalStatusStdout.Bytes(), &finalStatus); err != nil {
		t.Fatal(err)
	}
	if !hasNodeState(finalStatus.Nodes, "backend_dev", api.StateStopped) {
		t.Fatalf("expected backend_dev stopped after stop command: %+v", finalStatus.Nodes)
	}
	if !hasNodeState(finalStatus.Nodes, "frontend_dev", api.StateStopped) {
		t.Fatalf("expected frontend_dev stopped after stop command: %+v", finalStatus.Nodes)
	}
}

func waitForProcessExit(t *testing.T, handle *process.Handle) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- handle.Wait()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for process exit")
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func hasNodeState(nodes []api.NodeStatus, name string, want api.NodeState) bool {
	for _, node := range nodes {
		if node.Name == name && node.State == want {
			return true
		}
	}
	return false
}

func containsInstance(items []api.InstanceSummary, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func recordCLITestSupervisor(t *testing.T, worktree string, run api.RunConfig) string {
	t.Helper()
	inst, err := instance.Resolve(worktree, filepath.Base(worktree))
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(worktree, ".devflow", "logs", inst.ID, "supervisor.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := instance.RecordDetachedRun(inst, run, os.Getpid(), logPath); err != nil {
		t.Fatal(err)
	}
	return inst.ID
}

func decodeCLIFlushResult(t *testing.T, data []byte) api.FlushResult {
	t.Helper()
	var result api.FlushResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode flush result: %v\n%s", err, string(data))
	}
	return result
}

func decodeJSONLines(t *testing.T, data []byte) []map[string]string {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	out := make([]map[string]string, 0, len(lines))
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var payload map[string]string
		if err := json.Unmarshal(line, &payload); err != nil {
			t.Fatalf("decode json line %q: %v", string(line), err)
		}
		out = append(out, payload)
	}
	return out
}

func seedExampleWorktree(dst string) error {
	root, err := filepath.Abs(filepath.Join("..", "..", "examples", "go-next-monorepo", "worktree"))
	if err != nil {
		return err
	}
	return copyTree(root, dst)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}
