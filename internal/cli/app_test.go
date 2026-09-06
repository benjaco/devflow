package cli

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	embeddedwebapp "github.com/benjaco/devflow/examples/embedded-web-app"
	_ "github.com/benjaco/devflow/examples/go-next-monorepo"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/cache"
	"github.com/benjaco/devflow/pkg/daemon"
	"github.com/benjaco/devflow/pkg/database"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

type failCLIProject struct{}
type taskTargetCLIProject struct{}
type depsCLIProject struct{}
type targetDepsCLIProject struct{}
type graphExplainCLIProject struct{}
type actionCLIProject struct{}
type validationCLIProject struct{}
type manifestCLIProject struct{}
type repairCLIProject struct{}

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
				for line := 1; line <= 360; line++ {
					switch line {
					case 118:
						rt.EmitLogLine("stderr", "--- FAIL: TestCMNavigatorAssertion (0.01s)")
					case 121:
						rt.EmitLogLine("stderr", "expected: 215")
					case 122:
						rt.EmitLogLine("stderr", "actual: 229.09458")
					case 124:
						rt.EmitLogLine("stderr", "Error: assertion values differ")
					default:
						rt.EmitLogLine("stderr", fmt.Sprintf("cleanup or passing package line %03d", line))
					}
				}
				rt.EmitLogLine("stderr", "implementator failure details")
				return fmt.Errorf("boom")
			},
		},
	}
}

func (failCLIProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"fail"}}}
}

func (taskTargetCLIProject) Name() string { return "cli-task-target-project" }

func (manifestCLIProject) Name() string { return "cli-manifest-project" }

func (manifestCLIProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "cli-manifest"}, nil
}

func (manifestCLIProject) Tasks() []project.Task {
	return []project.Task{{
		Name:        "build",
		Kind:        project.KindOnce,
		Cache:       true,
		RequiredEnv: []string{"DEVFLOW_MANIFEST_COUNTER_FILE", "DATABASE_URL"},
		Inputs: project.Inputs{
			Files: []string{"input.txt"},
			Custom: []project.FingerprintFunc{func(context.Context, *project.Runtime) (string, error) {
				file, err := os.OpenFile(os.Getenv("DEVFLOW_MANIFEST_COUNTER_FILE"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
				if err != nil {
					return "", err
				}
				if _, err := file.WriteString("called\n"); err != nil {
					_ = file.Close()
					return "", err
				}
				if err := file.Close(); err != nil {
					return "", err
				}
				return os.Getenv("DATABASE_URL"), nil
			}},
		},
		Outputs: project.Outputs{Files: []string{"out.txt"}},
		Run: func(_ context.Context, rt *project.Runtime) error {
			data, err := os.ReadFile(rt.Abs("input.txt"))
			if err != nil {
				return err
			}
			return os.WriteFile(rt.Abs("out.txt"), data, 0o644)
		},
	}}
}

func (manifestCLIProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"build"}}}
}

func (repairCLIProject) Name() string { return "cli-repair-project" }

func (repairCLIProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "cli-repair"}, nil
}

func (repairCLIProject) Tasks() []project.Task {
	writePermitted := func(rt *project.Runtime) error {
		if err := os.WriteFile(rt.Abs("frontend/app.txt"), []byte("repaired frontend\n"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(rt.Abs("backend/generated/model.sql.go"), []byte("// repaired generated Go\n"), 0o644)
	}
	writeLineEndings := func(rt *project.Runtime, paths ...string) error {
		contents := map[string]string{
			"frontend/app.txt": "original frontend\r\n",
			"outside.txt":      "original outside\r\n",
		}
		for _, path := range paths {
			if err := os.WriteFile(rt.Abs(path), []byte(contents[path]), 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	stageFixturePath := func(ctx context.Context, rt *project.Runtime, path string) error {
		cmd := exec.CommandContext(ctx, "git", "add", "--", path)
		cmd.Dir = rt.Worktree
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("stage fixture path %s: %w: %s", path, err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	return []project.Task{
		{
			Name: "repair_no_changes",
			Kind: project.KindOnce,
			Run:  func(context.Context, *project.Runtime) error { return nil },
		},
		{
			Name: "repair_changes",
			Kind: project.KindOnce,
			Run: func(_ context.Context, rt *project.Runtime) error {
				return writePermitted(rt)
			},
		},
		{
			Name: "repair_changes_with_untracked",
			Kind: project.KindOnce,
			Run: func(_ context.Context, rt *project.Runtime) error {
				if err := writePermitted(rt); err != nil {
					return err
				}
				return os.WriteFile(rt.Abs("outside-untracked.txt"), []byte("untracked outside permitted paths\n"), 0o644)
			},
		},
		{
			Name: "repair_line_endings",
			Kind: project.KindOnce,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				if err := writeLineEndings(rt, "frontend/app.txt"); err != nil {
					return err
				}
				return stageFixturePath(ctx, rt, "frontend/app.txt")
			},
		},
		{
			Name: "repair_line_endings_mixed",
			Kind: project.KindOnce,
			Run: func(_ context.Context, rt *project.Runtime) error {
				if err := writeLineEndings(rt, "frontend/app.txt", "outside.txt"); err != nil {
					return err
				}
				return os.WriteFile(rt.Abs("backend/generated/model.sql.go"), []byte("// repaired generated Go\n"), 0o644)
			},
		},
		{
			Name: "repair_line_endings_with_content",
			Kind: project.KindOnce,
			Run: func(_ context.Context, rt *project.Runtime) error {
				return os.WriteFile(rt.Abs("frontend/app.txt"), []byte("repaired frontend\r\n"), 0o644)
			},
		},
		{
			Name: "repair_then_fail",
			Kind: project.KindOnce,
			Run: func(_ context.Context, rt *project.Runtime) error {
				if err := writePermitted(rt); err != nil {
					return err
				}
				return fmt.Errorf("repair DAG failure")
			},
		},
		{
			Name: "repair_unexpected",
			Kind: project.KindOnce,
			Run: func(_ context.Context, rt *project.Runtime) error {
				if err := writePermitted(rt); err != nil {
					return err
				}
				return os.WriteFile(rt.Abs("outside.txt"), []byte("unexpected tracked change\n"), 0o644)
			},
		},
	}
}

func (repairCLIProject) Targets() []project.Target {
	return []project.Target{
		{Name: "repair-no-changes", RootTasks: []string{"repair_no_changes"}},
		{Name: "repair-changes", RootTasks: []string{"repair_changes"}},
		{Name: "repair-changes-with-untracked", RootTasks: []string{"repair_changes_with_untracked"}},
		{Name: "repair-line-endings", RootTasks: []string{"repair_line_endings"}},
		{Name: "repair-line-endings-mixed", RootTasks: []string{"repair_line_endings_mixed"}},
		{Name: "repair-line-endings-with-content", RootTasks: []string{"repair_line_endings_with_content"}},
		{Name: "repair-fails", RootTasks: []string{"repair_then_fail"}},
		{Name: "repair-unexpected", RootTasks: []string{"repair_unexpected"}},
	}
}

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

func (depsCLIProject) Tasks() []project.Task {
	return []project.Task{
		{Name: "shell_task", Kind: project.KindOnce, RequiredCLIs: []string{"shell"}},
		{Name: "installable_task", Kind: project.KindOnce, RequiredCLIs: []string{"missing-tool"}},
	}
}

func (depsCLIProject) Targets() []project.Target {
	return []project.Target{
		{Name: "noop", RootTasks: []string{"shell_task"}},
		{Name: "installable", RootTasks: []string{"installable_task"}},
	}
}

func (depsCLIProject) RequiredCLIs() []project.RequiredCLI {
	marker := filepath.Join(os.TempDir(), "devflow-cli-deps-installed.txt")
	command := cliMissingToolCommand()
	bin := filepath.Join(os.TempDir(), command)
	installer := project.InstallScript{Script: strings.Join([]string{
		"echo installed > " + shellQuote(marker),
		"cat > " + shellQuote(bin) + " <<'EOF'",
		"#!/bin/sh",
		"exit 0",
		"EOF",
		"chmod +x " + shellQuote(bin),
	}, "\n")}
	if runtime.GOOS == "windows" {
		bin = filepath.Join(os.TempDir(), command)
		installer = project.InstallScript{
			Shell: "powershell",
			Script: strings.Join([]string{
				"Set-Content -Path " + shellQuote(marker) + " -Value installed",
				"Set-Content -Path " + shellQuote(bin) + " -Value '@echo off`r`nexit /b 0'",
			}, "\n"),
		}
	}
	return []project.RequiredCLI{
		{Name: "shell", Command: "go"},
		{
			Name:    "missing-tool",
			Command: command,
			Install: map[string]project.InstallScript{runtime.GOOS: installer},
		},
	}
}

func cliMissingToolCommand() string {
	if runtime.GOOS == "windows" {
		return "devflow-cli-missing-tool.cmd"
	}
	return "devflow-cli-missing-tool"
}

func (targetDepsCLIProject) Name() string { return "cli-target-deps-project" }

func (targetDepsCLIProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "cli-target-deps", Env: map[string]string{"UP_TOKEN": "dotenv-up"}}, nil
}

func (targetDepsCLIProject) Tasks() []project.Task {
	return []project.Task{
		{Name: "serve", Kind: project.KindOnce, RequiredCLIs: []string{"shell"}, RequiredEnv: []string{"UP_TOKEN"}},
		{Name: "deploy", Kind: project.KindOnce, RequiredCLIs: []string{"deploy-tool"}, RequiredEnv: []string{"DEV_DATABASE_URL"}},
	}
}

func (targetDepsCLIProject) Targets() []project.Target {
	return []project.Target{
		{Name: "up", RootTasks: []string{"serve"}},
		{Name: "deploy", RootTasks: []string{"deploy"}, RequiredCLIs: []string{"cloud"}},
	}
}

func (targetDepsCLIProject) RequiredCLIs() []project.RequiredCLI {
	return []project.RequiredCLI{
		{Name: "cloud", Command: "devflow-cli-definitely-missing-cloud-tool"},
		{Name: "deploy-tool", Command: "devflow-cli-definitely-missing-deploy-tool"},
		{Name: "shell", Command: "go"},
	}
}

func (graphExplainCLIProject) Name() string { return "cli-graph-explain-project" }

func (graphExplainCLIProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "cli-graph-explain"}, nil
}

func (graphExplainCLIProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:   "codegen",
			Kind:   project.KindOnce,
			Inputs: project.Inputs{Files: []string{"schema.json"}},
		},
		{
			Name: "build",
			Kind: project.KindOnce,
			Deps: []string{"codegen"},
			Inputs: project.Inputs{
				Dirs:   []string{"internal/storage"},
				Ignore: []string{"sqlc"},
			},
		},
	}
}

func (graphExplainCLIProject) Targets() []project.Target {
	return []project.Target{{Name: "up", RootTasks: []string{"build"}}}
}

func (actionCLIProject) Name() string { return "cli-action-project" }

func (actionCLIProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "cli-action"}, nil
}

func (actionCLIProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name: "create_migration",
			Kind: project.KindOnce,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				return os.WriteFile(filepath.Join(rt.Worktree, "action.txt"), []byte(rt.Env["MIGRATION_NAME"]), 0o644)
			},
		},
	}
}

func (actionCLIProject) Targets() []project.Target {
	return []project.Target{{Name: "up", RootTasks: []string{"create_migration"}}}
}

func (actionCLIProject) Actions() []project.Action {
	return []project.Action{
		{
			ID:        "db.migration.create",
			Kind:      database.ActionMigrationCreate,
			Category:  project.ActionCategoryAuthoring,
			Label:     "Create test migration",
			Component: "db",
			Task:      "create_migration",
			Inputs: []project.ActionInput{
				{Name: "name", Type: project.ActionInputString, Required: true, Positional: true, Env: "MIGRATION_NAME"},
			},
			Effects:  project.ActionEffects{Writes: []string{"action.txt"}},
			Relaunch: project.ActionRelaunchNever,
			Aliases:  []string{"db:migration:create"},
		},
	}
}

func (validationCLIProject) Name() string { return "cli-validation-project" }

func (validationCLIProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "cli-validation"}, nil
}

func (validationCLIProject) Tasks() []project.Task {
	writeFromSource := func(name string) project.Task {
		return project.Task{
			Name:    name,
			Kind:    project.KindOnce,
			Inputs:  project.Inputs{Files: []string{"source.txt"}},
			Outputs: project.Outputs{Files: []string{name + ".txt"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				data, err := os.ReadFile(rt.Abs("source.txt"))
				if err != nil {
					return err
				}
				return project.WriteFile(rt, name+".txt", append(data, name...), 0o644)
			},
		}
	}
	return []project.Task{
		writeFromSource("a"),
		writeFromSource("b"),
		{
			Name:    "bad",
			Kind:    project.KindOnce,
			Outputs: project.Outputs{Files: []string{"expected.txt"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				return project.WriteFile(rt, "surprise.txt", []byte("unexpected"), 0o644)
			},
		},
	}
}

func (validationCLIProject) Targets() []project.Target {
	return []project.Target{
		{Name: "build", RootTasks: []string{"a", "b"}},
		{Name: "bad", RootTasks: []string{"bad"}},
	}
}

func init() {
	daemon.SetStartDaemonFuncForTest(func(worktree, instanceID, projectName string) error {
		logPath := filepath.Join(worktree, ".devflow", "logs", instanceID, "daemon.log")
		go func() {
			_ = daemon.Serve(context.Background(), daemon.Options{Worktree: worktree, Project: projectName, LogPath: logPath})
		}()
		return nil
	})
	project.Register(failCLIProject{})
	project.Register(taskTargetCLIProject{})
	project.Register(depsCLIProject{})
	project.Register(targetDepsCLIProject{})
	project.Register(graphExplainCLIProject{})
	project.Register(actionCLIProject{})
	project.Register(validationCLIProject{})
	project.Register(manifestCLIProject{})
	project.Register(repairCLIProject{})
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

func TestActionListJSON(t *testing.T) {
	worktree := t.TempDir()
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"action", "list", "--json", "--project", "cli-action-project", "--worktree", worktree}); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Project string `json:"project"`
		Actions []struct {
			ID        string `json:"id"`
			Kind      string `json:"kind"`
			Component string `json:"component"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(app.Stdout.(*bytes.Buffer).Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Project != "cli-action-project" || len(payload.Actions) != 1 {
		t.Fatalf("unexpected action list: %+v", payload)
	}
	if payload.Actions[0].ID != "db.migration.create" || payload.Actions[0].Kind != database.ActionMigrationCreate || payload.Actions[0].Component != "db" {
		t.Fatalf("unexpected action payload: %+v", payload.Actions[0])
	}
}

func TestMigrationCreateRunsActionJSON(t *testing.T) {
	worktree := t.TempDir()
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	err := app.Run([]string{"migration", "create", "add-user", "--json", "--project", "cli-action-project", "--worktree", worktree})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		ActionID string            `json:"actionId"`
		Kind     string            `json:"kind"`
		Status   string            `json:"status"`
		Inputs   map[string]string `json:"inputs"`
		Created  []string          `json:"createdFiles"`
		Run      *api.RunResult    `json:"run"`
	}
	if err := json.Unmarshal(app.Stdout.(*bytes.Buffer).Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ActionID != "db.migration.create" || payload.Kind != database.ActionMigrationCreate || payload.Status != "succeeded" {
		t.Fatalf("unexpected action result: %+v", payload)
	}
	if payload.Inputs["name"] != "add-user" {
		t.Fatalf("unexpected inputs: %+v", payload.Inputs)
	}
	if len(payload.Created) != 1 || payload.Created[0] != "action.txt" {
		t.Fatalf("unexpected created files: %+v", payload.Created)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "action.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "add-user" {
		t.Fatalf("unexpected action output %q", string(data))
	}
}

func TestGraphAffectedExplainJSON(t *testing.T) {
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{
		"graph", "affected",
		"--json",
		"--explain",
		"--project", "cli-graph-explain-project",
		"--files", "schema.json,internal/storage/sqlc/users.sql.go,unmatched.txt",
	}); err != nil {
		t.Fatal(err)
	}
	var result api.GraphAffectedResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.DirectlyAffected, ",") != "codegen" {
		t.Fatalf("unexpected directly affected tasks: %+v", result)
	}
	if strings.Join(result.Downstream, ",") != "build,codegen" {
		t.Fatalf("unexpected downstream tasks: %+v", result)
	}
	if len(result.Explanations) != 2 {
		t.Fatalf("unexpected explanations: %+v", result.Explanations)
	}
	if result.Explanations[0].File != "internal/storage/sqlc/users.sql.go" || result.Explanations[0].Task != "build" || result.Explanations[0].Affected || result.Explanations[0].Reason != "ignored" || result.Explanations[0].Ignore != "sqlc" {
		t.Fatalf("unexpected ignored explanation: %+v", result.Explanations[0])
	}
	if result.Explanations[1].File != "schema.json" || result.Explanations[1].Task != "codegen" || !result.Explanations[1].Affected || result.Explanations[1].Reason != "file" {
		t.Fatalf("unexpected file explanation: %+v", result.Explanations[1])
	}
	if strings.Join(result.UnmatchedFiles, ",") != "unmatched.txt" {
		t.Fatalf("unexpected unmatched files: %+v", result.UnmatchedFiles)
	}
}

func TestRunHelpDescribesOperationalFlags(t *testing.T) {
	stderr := &bytes.Buffer{}
	app := &App{Stdout: &bytes.Buffer{}, Stderr: stderr}
	err := app.Run([]string{"run", "--help"})
	if err == nil {
		t.Fatal("expected help error")
	}
	help := stderr.String()
	for _, want := range []string{"--ci", "finite CI/readiness probe", "--detach", "not a readiness gate", "--watch", "--commit-changes", "--commit-path", "--fail-after-commit", "--pedantic"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help output missing %q:\n%s", want, help)
		}
	}
}

func TestRunRepositoryRepairFlagsRequireExplicitFiniteConfiguration(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "push-needs-mode", args: []string{"run", "build", "--push"}, want: "require --commit-changes"},
		{name: "pedantic-needs-mode", args: []string{"run", "build", "--pedantic"}, want: "require --commit-changes"},
		{name: "mode-needs-ci", args: []string{"run", "build", "--commit-changes", "--commit-path", "frontend", "--commit-message", "repair"}, want: "only with run --ci"},
		{name: "mode-needs-path", args: []string{"run", "build", "--ci", "--commit-changes", "--commit-message", "repair"}, want: "at least one --commit-path"},
		{name: "mode-needs-message", args: []string{"run", "build", "--ci", "--commit-changes", "--commit-path", "frontend"}, want: "non-empty --commit-message"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
			err := app.Run(test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateAllJSON(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "source.txt"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{
		"validate", "build",
		"--mode", "all",
		"--max-orders", "10",
		"--json",
		"--project", "cli-validation-project",
		"--worktree", worktree,
	}); err != nil {
		t.Fatalf("validate failed: %v\n%s", err, stderr.String())
	}
	var result api.ValidationResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode validation JSON: %v\n%s", err, stdout.String())
	}
	if !result.Success || result.Mode != api.ValidationModeAll {
		t.Fatalf("unexpected validation result: %+v", result)
	}
	if result.Details != api.ValidationDetailsIssues || result.Metrics.TotalFilesProcessed == 0 {
		t.Fatalf("missing bounded validation details/metrics: %+v", result)
	}
	if !strings.Contains(stderr.String(), "phase=Preparing validation") || !strings.Contains(stderr.String(), "phase=Cleaning up") {
		t.Fatalf("validation progress was not streamed to stderr: %s", stderr.String())
	}
	if result.Artifacts == nil || len(result.Artifacts.Tasks) != 2 {
		t.Fatalf("unexpected artifact result: %+v", result.Artifacts)
	}
	if result.Orders == nil || !result.Orders.Complete || result.Orders.TotalOrders != 2 || len(result.Orders.Runs) != 2 {
		t.Fatalf("unexpected order result: %+v", result.Orders)
	}
	if _, err := os.Stat(filepath.Join(worktree, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("validation changed the real worktree: %v", err)
	}
}

func TestValidateFailureStillEmitsJSON(t *testing.T) {
	worktree := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run([]string{
		"validate", "bad",
		"--mode", "artifacts",
		"--json",
		"--project", "cli-validation-project",
		"--worktree", worktree,
	})
	if err == nil {
		t.Fatalf("expected validation command failure")
	}
	var result api.ValidationResult
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatalf("decode validation failure JSON: %v\n%s", decodeErr, stdout.String())
	}
	if result.Success || result.Artifacts == nil || result.Artifacts.Success {
		t.Fatalf("unexpected validation failure result: %+v", result)
	}
	if len(result.Issues) == 0 || result.Issues[0].Kind == "" {
		t.Fatalf("expected aggregated validation issues: %+v", result.Issues)
	}
	task := result.Artifacts.Tasks[0]
	if task.UndeclaredWriteCount != 1 || len(result.Artifacts.Samples.UndeclaredWrites) != 1 || result.Artifacts.Samples.UndeclaredWrites[0] != "surprise.txt" {
		t.Fatalf("unexpected undeclared writes: %+v", task)
	}
}

func TestValidateRejectsNonPositiveMaxOrders(t *testing.T) {
	app := &App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.Run([]string{"validate", "build", "--max-orders", "0", "--project", "cli-validation-project", "--worktree", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "--max-orders must be positive") {
		t.Fatalf("unexpected error: %v", err)
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

func TestDocsRequiresScopedBundle(t *testing.T) {
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	err := app.Run([]string{"docs"})
	if err == nil {
		t.Fatal("expected bare docs command to fail")
	}
	if !strings.Contains(err.Error(), "devflow docs <setup|development>") {
		t.Fatalf("unexpected docs error: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("bare docs should not print bundled docs, got %q", stdout.String())
	}
}

func TestDocsSetupPrintsSetupMarkdownOnly(t *testing.T) {
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"docs", "setup"}); err != nil {
		t.Fatal(err)
	}
	output := stdout.String()
	assertDocsMarkersInOrder(t, output, []string{
		"<!-- docs_users/setup.md -->",
		"# Devflow Setup Docs",
		"<!-- docs_users/adapter-guide.md -->",
		"# Adapter Guide",
	})
	if !strings.Contains(output, "devflow validate build --mode all --json") {
		t.Fatalf("setup docs did not include pipeline validation guidance")
	}
	for _, forbidden := range []string{
		"<!-- docs_users/development.md -->",
		"# Devflow Development Docs",
		"<!-- docs_users/agent-integration.md -->",
		"# Agent Integration",
		"# Developing Devflow",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("setup docs included forbidden content %q", forbidden)
		}
	}
}

func TestDocsDevelopmentPrintsDevelopmentMarkdownOnly(t *testing.T) {
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"docs", "development"}); err != nil {
		t.Fatal(err)
	}
	output := stdout.String()
	assertDocsMarkersInOrder(t, output, []string{
		"<!-- docs_users/development.md -->",
		"# Devflow Development Docs",
		"<!-- docs_users/agent-integration.md -->",
		"# Agent Integration",
	})
	if !strings.Contains(output, "devflow validate build --mode all --json") {
		t.Fatalf("development docs did not include pipeline validation guidance")
	}
	for _, forbidden := range []string{
		"<!-- docs_users/setup.md -->",
		"# Devflow Setup Docs",
		"<!-- docs_users/adapter-guide.md -->",
		"# Adapter Guide",
		"# Developing Devflow",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("development docs included forbidden content %q", forbidden)
		}
	}
}

func assertDocsMarkersInOrder(t *testing.T, output string, markers []string) {
	t.Helper()
	last := -1
	for _, marker := range markers {
		idx := strings.Index(output, marker)
		if idx < 0 {
			t.Fatalf("missing docs marker %q in output", marker)
		}
		if idx <= last {
			t.Fatalf("marker %q appeared out of order", marker)
		}
		last = idx
	}
}

type notifyingBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	needle string
	seen   chan struct{}
	once   sync.Once
}

func newNotifyingBuffer(needle string) *notifyingBuffer {
	return &notifyingBuffer{needle: needle, seen: make(chan struct{})}
}

func (b *notifyingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buffer.Write(p)
	matched := strings.Contains(b.buffer.String(), b.needle)
	b.mu.Unlock()
	if matched {
		b.once.Do(func() { close(b.seen) })
	}
	return n, err
}

func (b *notifyingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestUpgradeTextStreamsGoInstallOutputBeforeCompletion(t *testing.T) {
	installFakeGo(t, 0)
	t.Setenv("DEVFLOW_FAKE_GO_UPGRADE_DELAY", "500ms")
	stream := newNotifyingBuffer("fake go output")
	app := &App{Stdout: stream, Stderr: stream}
	done := make(chan error, 1)
	go func() {
		done <- app.Run([]string{"upgrade"})
	}()

	select {
	case <-stream.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for streamed go install output")
	}
	select {
	case err := <-done:
		t.Fatalf("upgrade completed before its child output was observed: %v", err)
	default:
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upgrade completion")
	}
	output := stream.String()
	for _, want := range []string{"[devflow] upgrade started", "fake go output", "[devflow] upgrade finished success=true", "upgraded devflow using go install"} {
		if !strings.Contains(output, want) {
			t.Fatalf("streamed text output lacks %q:\n%s", want, output)
		}
	}
}

func TestUpgradeJSONStreamsToStderrAndKeepsStdoutMachineClean(t *testing.T) {
	installFakeGo(t, 0)
	t.Setenv("DEVFLOW_FAKE_GO_UPGRADE_DELAY", "500ms")
	stdout := &bytes.Buffer{}
	stderr := newNotifyingBuffer("fake go output")
	app := &App{Stdout: stdout, Stderr: stderr}
	done := make(chan error, 1)
	go func() {
		done <- app.Run([]string{"upgrade", "--json"})
	}()

	select {
	case <-stderr.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for JSON-mode streamed go install output")
	}
	select {
	case err := <-done:
		t.Fatalf("JSON upgrade completed before its child output was observed: %v", err)
	default:
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for JSON upgrade completion")
	}

	var result api.UpgradeResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode upgrade JSON: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if !result.Success || !strings.Contains(result.Output, "fake go output") {
		t.Fatalf("upgrade JSON lost captured child output: %+v", result)
	}
	if strings.Contains(stdout.String(), "[devflow]") {
		t.Fatalf("upgrade progress leaked onto JSON stdout: %s", stdout.String())
	}
	progress := stderr.String()
	if !strings.Contains(progress, "[devflow] upgrade started") || !strings.Contains(progress, "[devflow] upgrade finished success=true") {
		t.Fatalf("upgrade progress was not streamed to stderr: %s", progress)
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
	proxy, err := os.ReadFile(argsPath + ".goproxy")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(proxy)); got == "direct" {
		t.Fatalf("default upgrade should not force GOPROXY=direct")
	}
	var result api.UpgradeResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.VersionTarget != "latest" {
		t.Fatalf("unexpected upgrade result: %+v", result)
	}
}

func TestUpgradeDirectJSONRunsGoInstallWithDirectProxy(t *testing.T) {
	argsPath := installFakeGo(t, 0)
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	t.Setenv("GOPROXY", "https://proxy.golang.org,direct")
	if err := app.Run([]string{"upgrade", "--json", "--direct"}); err != nil {
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
	proxy, err := os.ReadFile(argsPath + ".goproxy")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(proxy)) != "direct" {
		t.Fatalf("expected --direct to force GOPROXY=direct, got %q", string(proxy))
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
	if result.Success || result.Error == nil || result.Error.Message == "" {
		t.Fatalf("expected structured failure, got %+v", result)
	}
}

func hasString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestUpgradeGoEnvForcesDirectProxy(t *testing.T) {
	env := upgradeGoEnv([]string{"PATH=/bin", "GOPROXY=https://proxy.golang.org,direct"})
	if !hasString(env, "GOPROXY=direct") {
		t.Fatalf("expected direct proxy env, got %+v", env)
	}
	for _, item := range env {
		if item == "GOPROXY=https://proxy.golang.org,direct" {
			t.Fatalf("expected original GOPROXY to be replaced, got %+v", env)
		}
	}
	env = upgradeGoEnv([]string{"PATH=/bin"})
	if !hasString(env, "GOPROXY=direct") {
		t.Fatalf("expected direct proxy env to be added, got %+v", env)
	}
}

func TestUpgradePathWarningWhenPathShadowsGoInstall(t *testing.T) {
	dir := t.TempDir()
	goPath := filepath.Join(dir, "gopath")
	installedDir := filepath.Join(goPath, "bin")
	shadowDir := filepath.Join(dir, "shadow")
	if err := os.MkdirAll(installedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shadowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installedDir, devflowExecutableName()), []byte("installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shadowDir, devflowExecutableName()), []byte("shadow"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeGo := buildFakeGoCommand(t, dir, "env", 0)
	t.Setenv("DEVFLOW_TEST_GOPATH", goPath)
	t.Setenv("PATH", shadowDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	warning := upgradePathWarning(fakeGo)
	if !strings.Contains(warning, "go install wrote") || !strings.Contains(warning, installedDir) || !strings.Contains(warning, shadowDir) {
		t.Fatalf("expected shadow warning, got %q", warning)
	}
}

func TestUpgradePathWarningSkipsWhenInstalledBinaryIsOnPath(t *testing.T) {
	dir := t.TempDir()
	goPath := filepath.Join(dir, "gopath")
	installedDir := filepath.Join(goPath, "bin")
	if err := os.MkdirAll(installedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installedDir, devflowExecutableName()), []byte("installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeGo := buildFakeGoCommand(t, dir, "env", 0)
	t.Setenv("DEVFLOW_TEST_GOPATH", goPath)
	t.Setenv("PATH", installedDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if warning := upgradePathWarning(fakeGo); warning != "" {
		t.Fatalf("did not expect warning when installed devflow is on PATH, got %q", warning)
	}
}

func TestRunJSONStillReturnsExecutionError(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: stderr}
	err := app.Run([]string{"run", "build", "--json", "--ci", "--project", "cli-fail-project", "--worktree", t.TempDir()})
	if err == nil {
		t.Fatal("expected run command to return task failure even with --json")
	}
	var result api.RunResult
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatalf("decode failure JSON: %v\nstdout=%s\nstderr=%s", decodeErr, stdout.String(), stderr.String())
	}
	if result.Success || result.Error == nil || result.Error.Message != "boom" || result.FailedNode != "fail" || result.FailedNodeLogPath == "" {
		t.Fatalf("failure JSON lacks actionable details: %+v", result)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].State != api.StateFailed || result.Nodes[0].DurationMs <= 0 {
		t.Fatalf("failure JSON lacks per-node state/duration: %+v", result.Nodes)
	}
	if !strings.Contains(strings.Join(result.LogTail, "\n"), "implementator failure details") {
		t.Fatalf("failure JSON lacks bounded log tail: %+v", result.LogTail)
	}
	if strings.Contains(strings.Join(result.LogTail, "\n"), "expected: 215") {
		t.Fatalf("terminal tail unexpectedly contains the early assertion: %+v", result.LogTail)
	}
	if len(result.FailureExcerpts) == 0 || result.FailureExcerpts[0].Reason != "go-test-failure" || !strings.Contains(strings.Join(result.FailureExcerpts[0].Lines, "\n"), "expected: 215") {
		t.Fatalf("failure JSON lacks the early assertion excerpt: %+v", result.FailureExcerpts)
	}
	if len(stdout.Bytes()) > 96*1024 {
		t.Fatalf("bounded failure JSON unexpectedly large: %d bytes", len(stdout.Bytes()))
	}
	progress := stderr.String()
	if got := strings.Count(progress, "[devflow] fail stderr:"); got != 361 {
		t.Fatalf("CI JSON progress emitted %d task log events, want 361:\n%s", got, progress)
	}
	if !strings.Contains(progress, "[devflow] run build started") || !strings.Contains(progress, "[devflow] task fail: failed: boom") || !strings.Contains(progress, "implementator failure details") {
		t.Fatalf("CI JSON progress was not streamed to stderr: %s", progress)
	}
}

func TestRunRepositoryRepairNoChangesReturnsNormalSuccess(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	before := repairGitText(t, worktree, "rev-parse", "HEAD")
	result, stdout, stderr, runErr := runRepositoryRepair(t, worktree, "repair-no-changes", "--push", "--fail-after-commit")
	if runErr != nil {
		t.Fatalf("no-change repair failed: %v\nstdout=%s\nstderr=%s", runErr, stdout, stderr)
	}
	if !result.Success || result.RepositoryChanges == nil {
		t.Fatalf("missing successful repository result: %+v", result)
	}
	repository := result.RepositoryChanges
	if repository.Status != api.RepositoryChangeStatusNoChanges || repository.ChangedPathCount != 0 || repository.CommitCreated || repository.PushAttempted || repository.PushSucceeded {
		t.Fatalf("unexpected no-change repository result: %+v", repository)
	}
	if !repository.FailAfterCommitRequested || repository.FailAfterCommitTriggered {
		t.Fatalf("fail-after-commit should not trigger without a commit: %+v", repository)
	}
	if after := repairGitText(t, worktree, "rev-parse", "HEAD"); after != before {
		t.Fatalf("no-change run advanced HEAD: before=%s after=%s", before, after)
	}
	if !strings.Contains(stderr, "repository repair: no permitted changes found") || strings.Contains(stdout, "[devflow]") {
		t.Fatalf("repository progress was not isolated to stderr:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	var wire map[string]any
	if err := json.Unmarshal([]byte(stdout), &wire); err != nil {
		t.Fatal(err)
	}
	repositoryWire, ok := wire["repositoryChanges"].(map[string]any)
	if !ok {
		t.Fatalf("repositoryChanges is not an object: %s", stdout)
	}
	for _, field := range []string{"status", "pedantic", "changedPaths", "changedPathCount", "changedPathsTruncated", "ignoredLineEndingPaths", "ignoredLineEndingPathCount", "ignoredLineEndingPathsTruncated", "unexpectedTrackedPaths", "unexpectedTrackedPathCount", "unexpectedTrackedPathsTruncated", "commitCreated", "commitSha", "pushAttempted", "pushSucceeded", "failAfterCommitRequested", "failAfterCommitTriggered"} {
		if _, ok := repositoryWire[field]; !ok {
			t.Fatalf("repositoryChanges omitted stable field %q: %s", field, stdout)
		}
	}
}

func TestRunRepositoryRepairIgnoresLineEndingOnlyChangesByDefault(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	before := repairGitText(t, worktree, "rev-parse", "HEAD")
	result, stdout, stderr, runErr := runRepositoryRepair(t, worktree, "repair-line-endings", "--push", "--fail-after-commit")
	if runErr != nil {
		t.Fatalf("line-ending-only repair failed: %v\nstdout=%s\nstderr=%s", runErr, stdout, stderr)
	}
	if !result.Success || result.RepositoryChanges == nil {
		t.Fatalf("missing successful repository result: %+v", result)
	}
	repository := result.RepositoryChanges
	if repository.Status != api.RepositoryChangeStatusNoChanges || repository.Pedantic || repository.ChangedPathCount != 0 || repository.CommitCreated || repository.PushAttempted {
		t.Fatalf("line-ending-only change triggered repository mutation: %+v", repository)
	}
	if repository.IgnoredLineEndingPathCount != 1 || strings.Join(repository.IgnoredLineEndingPaths, ",") != "frontend/app.txt" || repository.IgnoredLineEndingPathsTruncated {
		t.Fatalf("ignored line-ending detail missing: %+v", repository)
	}
	if !repository.FailAfterCommitRequested || repository.FailAfterCommitTriggered {
		t.Fatalf("line-ending-only change triggered deliberate failure: %+v", repository)
	}
	if after := repairGitText(t, worktree, "rev-parse", "HEAD"); after != before {
		t.Fatalf("line-ending-only change advanced HEAD: before=%s after=%s", before, after)
	}
	if got := repairGit(t, worktree, "show", "HEAD:frontend/app.txt"); got != "original frontend\n" {
		t.Fatalf("line-ending-only bytes entered HEAD: %q", got)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "frontend", "app.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "original frontend\r\n"; got != want {
		t.Fatalf("ignored worktree bytes = %q, want %q", got, want)
	}
	if got := repairGitText(t, worktree, "status", "--porcelain=v1"); got != " M frontend/app.txt" {
		t.Fatalf("ignored line-ending-only path was not left unstaged: %q", got)
	}
	if !strings.Contains(stderr, "ignoring 1 CRLF/LF-only tracked path") || !strings.Contains(stderr, "no permitted changes found") || strings.Contains(stdout, "[devflow]") {
		t.Fatalf("line-ending progress streams were incorrect:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
}

func TestRunRepositoryRepairPedanticCommitsLineEndingOnlyChanges(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	result, _, _, runErr := runRepositoryRepair(t, worktree, "repair-line-endings", "--pedantic")
	if runErr != nil {
		t.Fatalf("pedantic line-ending repair failed: %v", runErr)
	}
	if !result.Success || result.RepositoryChanges == nil {
		t.Fatalf("missing successful repository result: %+v", result)
	}
	repository := result.RepositoryChanges
	if repository.Status != api.RepositoryChangeStatusCommitted || !repository.Pedantic || !repository.CommitCreated || repository.ChangedPathCount != 1 || strings.Join(repository.ChangedPaths, ",") != "frontend/app.txt" {
		t.Fatalf("pedantic mode did not commit line-ending-only change: %+v", repository)
	}
	if repository.IgnoredLineEndingPathCount != 0 || len(repository.IgnoredLineEndingPaths) != 0 {
		t.Fatalf("pedantic mode reported ignored line endings: %+v", repository)
	}
	if got := repairGit(t, worktree, "show", "HEAD:frontend/app.txt"); got != "original frontend\r\n" {
		t.Fatalf("pedantic commit blob = %q", got)
	}
	if got := repairGitText(t, worktree, "status", "--porcelain=v1"); got != "" {
		t.Fatalf("pedantic commit left repository dirty: %q", got)
	}
}

func TestRunRepositoryRepairPedanticRejectsUnexpectedLineEndingChanges(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	before := repairGitText(t, worktree, "rev-parse", "HEAD")
	result, _, _, runErr := runRepositoryRepair(t, worktree, "repair-line-endings-mixed", "--pedantic")
	if runErr == nil {
		t.Fatal("expected pedantic unexpected-path failure")
	}
	if result.Success || result.RepositoryChanges == nil {
		t.Fatalf("unexpected pedantic failure result: %+v", result)
	}
	repository := result.RepositoryChanges
	if repository.Status != api.RepositoryChangeStatusUnexpectedTrackedChanges || !repository.Pedantic || repository.CommitCreated || repository.PushAttempted {
		t.Fatalf("pedantic mode did not reject unexpected line-ending change: %+v", repository)
	}
	if repository.ChangedPathCount != 2 || strings.Join(repository.ChangedPaths, ",") != "backend/generated/model.sql.go,frontend/app.txt" || repository.UnexpectedTrackedPathCount != 1 || strings.Join(repository.UnexpectedTrackedPaths, ",") != "outside.txt" {
		t.Fatalf("pedantic changed/unexpected paths were incorrect: %+v", repository)
	}
	if repository.IgnoredLineEndingPathCount != 0 {
		t.Fatalf("pedantic mode ignored a line-ending path: %+v", repository)
	}
	if after := repairGitText(t, worktree, "rev-parse", "HEAD"); after != before {
		t.Fatalf("pedantic unexpected-path failure advanced HEAD: before=%s after=%s", before, after)
	}
	if staged := repairGitText(t, worktree, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("pedantic unexpected-path failure staged paths: %q", staged)
	}
}

func TestRunRepositoryRepairExcludesLineEndingsFromMixedCommit(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	result, _, _, runErr := runRepositoryRepair(t, worktree, "repair-line-endings-mixed")
	if runErr != nil {
		t.Fatalf("mixed line-ending repair failed: %v", runErr)
	}
	if !result.Success || result.RepositoryChanges == nil {
		t.Fatalf("missing successful repository result: %+v", result)
	}
	repository := result.RepositoryChanges
	if repository.Status != api.RepositoryChangeStatusCommitted || !repository.CommitCreated || repository.ChangedPathCount != 1 || strings.Join(repository.ChangedPaths, ",") != "backend/generated/model.sql.go" {
		t.Fatalf("mixed repair committed the wrong paths: %+v", repository)
	}
	if repository.IgnoredLineEndingPathCount != 2 || strings.Join(repository.IgnoredLineEndingPaths, ",") != "frontend/app.txt,outside.txt" || repository.UnexpectedTrackedPathCount != 0 {
		t.Fatalf("mixed repair did not ignore permitted and unexpected line endings: %+v", repository)
	}
	if got := strings.Join(repairGitLines(t, worktree, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"), ","); got != "backend/generated/model.sql.go" {
		t.Fatalf("mixed repair commit paths = %q", got)
	}
	for path, want := range map[string]string{
		"frontend/app.txt": "original frontend\n",
		"outside.txt":      "original outside\n",
	} {
		if got := repairGit(t, worktree, "show", "HEAD:"+path); got != want {
			t.Fatalf("ignored line endings entered committed %s: %q", path, got)
		}
	}
	if got := strings.Join(repairGitLines(t, worktree, "status", "--porcelain=v1"), ","); got != " M frontend/app.txt, M outside.txt" {
		t.Fatalf("ignored mixed paths were not left unstaged: %q", got)
	}
}

func TestRunRepositoryRepairCommitsContentChangeWithDifferentLineEndings(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	result, _, _, runErr := runRepositoryRepair(t, worktree, "repair-line-endings-with-content")
	if runErr != nil {
		t.Fatalf("content-plus-line-ending repair failed: %v", runErr)
	}
	if !result.Success || result.RepositoryChanges == nil {
		t.Fatalf("missing successful repository result: %+v", result)
	}
	repository := result.RepositoryChanges
	if repository.Status != api.RepositoryChangeStatusCommitted || !repository.CommitCreated || repository.ChangedPathCount != 1 || strings.Join(repository.ChangedPaths, ",") != "frontend/app.txt" || repository.IgnoredLineEndingPathCount != 0 {
		t.Fatalf("real content change was ignored with its line endings: %+v", repository)
	}
	if got := repairGit(t, worktree, "show", "HEAD:frontend/app.txt"); got != "repaired frontend\r\n" {
		t.Fatalf("content-plus-line-ending commit blob = %q", got)
	}
}

func TestRunRepositoryRepairCommitsOnlyPermittedPathsWithHeadAttribution(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	before := repairGitText(t, worktree, "rev-parse", "HEAD")
	result, stdout, stderr, runErr := runRepositoryRepair(t, worktree, "repair-changes")
	if runErr != nil {
		t.Fatalf("changed repair failed: %v\nstdout=%s\nstderr=%s", runErr, stdout, stderr)
	}
	if !result.Success || result.RepositoryChanges == nil {
		t.Fatalf("missing successful repository result: %+v", result)
	}
	repository := result.RepositoryChanges
	if repository.Status != api.RepositoryChangeStatusCommitted || !repository.CommitCreated || repository.CommitSHA == "" || repository.PushAttempted {
		t.Fatalf("unexpected changed repository result: %+v", repository)
	}
	wantPaths := "backend/generated/model.sql.go,frontend/app.txt"
	if repository.ChangedPathCount != 2 || strings.Join(repository.ChangedPaths, ",") != wantPaths || repository.ChangedPathsTruncated {
		t.Fatalf("unexpected bounded changed paths: %+v", repository)
	}
	if head := repairGitText(t, worktree, "rev-parse", "HEAD"); head != repository.CommitSHA {
		t.Fatalf("reported commit %s does not match HEAD %s", repository.CommitSHA, head)
	}
	if parent := repairGitText(t, worktree, "rev-parse", "HEAD^"); parent != before {
		t.Fatalf("repair commit parent = %s, want %s", parent, before)
	}
	if paths := strings.Join(repairGitLines(t, worktree, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"), ","); paths != wantPaths {
		t.Fatalf("commit paths = %q, want %q", paths, wantPaths)
	}
	for path, want := range map[string]string{
		"frontend/app.txt":               "repaired frontend\n",
		"backend/generated/model.sql.go": "// repaired generated Go\n",
	} {
		if got := repairGit(t, worktree, "show", "HEAD:"+path); got != want {
			t.Fatalf("committed content for %s = %q, want %q", path, got, want)
		}
	}
	identity := repairGitText(t, worktree, "show", "-s", "--format=%an|%ae|%cn|%ce", "HEAD")
	if identity != "Head Author|head@example.invalid|Head Committer|committer@example.invalid" {
		t.Fatalf("repair identity was not derived from HEAD: %q", identity)
	}
	if message := repairGitText(t, worktree, "show", "-s", "--format=%s", "HEAD"); message != "bot(ci): automated Devflow formatting and generation" {
		t.Fatalf("unexpected repair commit message %q", message)
	}
	if status := repairGitText(t, worktree, "status", "--porcelain=v1"); status != "" {
		t.Fatalf("repair commit left repository dirty: %q", status)
	}
	if strings.Contains(stdout, "[devflow]") || !strings.Contains(stderr, "repository repair: created commit") {
		t.Fatalf("repository output streams were mixed:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
}

func TestRunRepositoryRepairLeavesOutOfPathspecUntrackedFileUncommitted(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	result, _, _, runErr := runRepositoryRepair(t, worktree, "repair-changes-with-untracked")
	if runErr != nil {
		t.Fatalf("repair with out-of-pathspec untracked file failed: %v", runErr)
	}
	if !result.Success || result.RepositoryChanges == nil {
		t.Fatalf("missing successful repository result: %+v", result)
	}
	repository := result.RepositoryChanges
	if repository.Status != api.RepositoryChangeStatusCommitted || !repository.CommitCreated {
		t.Fatalf("permitted repairs were not committed: %+v", repository)
	}
	if got := strings.Join(repository.ChangedPaths, ","); repository.ChangedPathCount != 2 || got != "backend/generated/model.sql.go,frontend/app.txt" {
		t.Fatalf("out-of-pathspec untracked file entered changed-path details: %+v", repository)
	}
	if got := repairGitText(t, worktree, "ls-tree", "-r", "--name-only", "HEAD", "--", "outside-untracked.txt"); got != "" {
		t.Fatalf("out-of-pathspec untracked file entered repair commit: %q", got)
	}
	if got := repairGitText(t, worktree, "status", "--porcelain=v1", "--untracked-files=all"); got != "?? outside-untracked.txt" {
		t.Fatalf("out-of-pathspec untracked file was not left in the worktree: %q", got)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "outside-untracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "untracked outside permitted paths\n"; got != want {
		t.Fatalf("out-of-pathspec untracked content = %q, want %q", got, want)
	}
}

func TestRunRepositoryRepairDAGFailureNeverCommitsOrPushes(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	before := repairGitText(t, worktree, "rev-parse", "HEAD")
	result, _, stderr, runErr := runRepositoryRepair(t, worktree, "repair-fails", "--push")
	if runErr == nil {
		t.Fatal("expected DAG failure")
	}
	if result.Success || result.Error == nil || result.Error.Message != "repair DAG failure" || result.RepositoryChanges == nil {
		t.Fatalf("unexpected DAG failure result: %+v", result)
	}
	repository := result.RepositoryChanges
	if repository.Status != api.RepositoryChangeStatusSkippedDAGFailed || repository.CommitCreated || repository.PushAttempted {
		t.Fatalf("DAG failure reached repository mutation: %+v", repository)
	}
	if after := repairGitText(t, worktree, "rev-parse", "HEAD"); after != before {
		t.Fatalf("DAG failure advanced HEAD: before=%s after=%s", before, after)
	}
	if staged := repairGitText(t, worktree, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("DAG failure staged paths: %q", staged)
	}
	if !strings.Contains(stderr, "repository repair: skipped because the DAG failed") {
		t.Fatalf("missing repository skip progress: %s", stderr)
	}
}

func TestRunRepositoryRepairRejectsUnexpectedTrackedPaths(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	before := repairGitText(t, worktree, "rev-parse", "HEAD")
	result, _, _, runErr := runRepositoryRepair(t, worktree, "repair-unexpected", "--push")
	if runErr == nil {
		t.Fatal("expected unexpected tracked change failure")
	}
	if result.Success || result.RepositoryChanges == nil {
		t.Fatalf("unexpected repository failure result: %+v", result)
	}
	repository := result.RepositoryChanges
	if repository.Status != api.RepositoryChangeStatusUnexpectedTrackedChanges || repository.CommitCreated || repository.PushAttempted {
		t.Fatalf("unexpected tracked path was not rejected before mutation: %+v", repository)
	}
	if repository.UnexpectedTrackedPathCount != 1 || strings.Join(repository.UnexpectedTrackedPaths, ",") != "outside.txt" {
		t.Fatalf("unexpected path detail missing: %+v", repository)
	}
	if repository.ChangedPathCount != 2 || strings.Join(repository.ChangedPaths, ",") != "backend/generated/model.sql.go,frontend/app.txt" {
		t.Fatalf("permitted change detail missing: %+v", repository)
	}
	if after := repairGitText(t, worktree, "rev-parse", "HEAD"); after != before {
		t.Fatalf("unexpected-path failure advanced HEAD: before=%s after=%s", before, after)
	}
	if staged := repairGitText(t, worktree, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("unexpected-path failure staged paths: %q", staged)
	}
}

func TestRunRepositoryRepairPushFailureReportsCreatedLocalCommit(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	missingRemote := filepath.ToSlash(filepath.Join(t.TempDir(), "missing-remote.git"))
	repairGit(t, worktree, "remote", "add", "origin", missingRemote)
	repairGit(t, worktree, "config", "push.default", "current")

	result, _, stderr, runErr := runRepositoryRepair(t, worktree, "repair-changes", "--push")
	if runErr == nil {
		t.Fatal("expected push failure")
	}
	if result.Success || result.RepositoryChanges == nil {
		t.Fatalf("unexpected push failure result: %+v", result)
	}
	repository := result.RepositoryChanges
	if repository.Status != api.RepositoryChangeStatusPushFailed || !repository.CommitCreated || repository.CommitSHA == "" || !repository.PushAttempted || repository.PushSucceeded {
		t.Fatalf("push failure did not preserve partial-success detail: %+v", repository)
	}
	if head := repairGitText(t, worktree, "rev-parse", "HEAD"); head != repository.CommitSHA {
		t.Fatalf("local commit was not retained after push failure: head=%s result=%+v", head, repository)
	}
	if result.Error == nil || !strings.Contains(result.Error.Message, "created locally, but git push failed") || !strings.Contains(stderr, "repository repair: pushing commit") {
		t.Fatalf("push partial failure was unclear:\nresult=%+v\nstderr=%s", result, stderr)
	}
}

func TestRunRepositoryRepairPushSucceedsBeforeDeliberateFailure(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	remote := filepath.ToSlash(filepath.Join(t.TempDir(), "remote.git"))
	repairGit(t, worktree, "init", "--bare", remote)
	repairGit(t, worktree, "remote", "add", "origin", remote)
	repairGit(t, worktree, "config", "push.default", "current")

	result, _, _, runErr := runRepositoryRepair(t, worktree, "repair-changes", "--push", "--fail-after-commit")
	if runErr == nil {
		t.Fatal("expected deliberate post-push failure")
	}
	if result.RepositoryChanges == nil {
		t.Fatalf("missing repository result: %+v", result)
	}
	repository := result.RepositoryChanges
	if repository.Status != api.RepositoryChangeStatusFailedAfterCommit || !repository.CommitCreated || !repository.PushAttempted || !repository.PushSucceeded || !repository.FailAfterCommitTriggered {
		t.Fatalf("push/fail-after ordering was not reported: %+v", repository)
	}
	branch := repairGitText(t, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	remoteHead := repairGitText(t, worktree, "--git-dir="+remote, "rev-parse", "refs/heads/"+branch)
	if remoteHead != repository.CommitSHA {
		t.Fatalf("remote commit = %s, want %s", remoteHead, repository.CommitSHA)
	}
}

func TestRunRepositoryRepairFailAfterCommitReturnsDeliberateFailure(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	result, _, _, runErr := runRepositoryRepair(t, worktree, "repair-changes", "--fail-after-commit")
	if runErr == nil {
		t.Fatal("expected deliberate fail-after-commit error")
	}
	if result.Success || result.RepositoryChanges == nil {
		t.Fatalf("unexpected fail-after-commit result: %+v", result)
	}
	repository := result.RepositoryChanges
	if repository.Status != api.RepositoryChangeStatusFailedAfterCommit || !repository.CommitCreated || !repository.FailAfterCommitRequested || !repository.FailAfterCommitTriggered || repository.PushAttempted {
		t.Fatalf("deliberate failure state missing: %+v", repository)
	}
	if head := repairGitText(t, worktree, "rev-parse", "HEAD"); head != repository.CommitSHA {
		t.Fatalf("fail-after-commit did not retain commit: head=%s result=%+v", head, repository)
	}
	if result.Error == nil || !strings.Contains(result.Error.Message, "failing deliberately") {
		t.Fatalf("deliberate error missing from final JSON: %+v", result)
	}
}

func TestRunRepositoryRepairRequiresCleanWorktreeBeforeDAG(t *testing.T) {
	worktree := initRepositoryRepairGitWorktree(t)
	if err := os.WriteFile(filepath.Join(worktree, "outside.txt"), []byte("dirty before run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeAllowed, err := os.ReadFile(filepath.Join(worktree, "frontend", "app.txt"))
	if err != nil {
		t.Fatal(err)
	}
	result, _, _, runErr := runRepositoryRepair(t, worktree, "repair-changes")
	if runErr == nil {
		t.Fatal("expected dirty-worktree precondition failure")
	}
	if result.Success || result.RepositoryChanges == nil || result.RepositoryChanges.Status != api.RepositoryChangeStatusPreconditionFailed {
		t.Fatalf("dirty precondition was not structured: %+v", result)
	}
	if result.RepositoryChanges.ChangedPathCount != 1 || strings.Join(result.RepositoryChanges.ChangedPaths, ",") != "outside.txt" {
		t.Fatalf("dirty path detail missing: %+v", result.RepositoryChanges)
	}
	afterAllowed, err := os.ReadFile(filepath.Join(worktree, "frontend", "app.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterAllowed, beforeAllowed) {
		t.Fatalf("DAG executed despite dirty precondition: before=%q after=%q", beforeAllowed, afterAllowed)
	}
}

func TestDetachedStartResultJSONPreservesAuthoritativeLaunchState(t *testing.T) {
	for _, test := range []struct {
		name  string
		start daemon.StartResult
	}{
		{
			name: "ready",
			start: daemon.StartResult{
				InstanceID:    "instance-ready",
				Target:        "dev",
				Mode:          api.ModeWatch,
				DaemonPID:     1234,
				LogPath:       "/tmp/daemon.log",
				Accepted:      true,
				DaemonStarted: true,
				Ready:         true,
				State:         "ready",
			},
		},
		{
			name: "slow-starting",
			start: daemon.StartResult{
				InstanceID:    "instance-starting",
				Target:        "dev",
				Mode:          api.ModeWatch,
				DaemonPID:     2345,
				Accepted:      true,
				DaemonStarted: true,
				Ready:         false,
				State:         "starting",
			},
		},
		{
			name: "already-failed",
			start: daemon.StartResult{
				InstanceID:    "instance-failed",
				Target:        "broken",
				Mode:          api.ModeWatch,
				DaemonPID:     3456,
				Accepted:      true,
				DaemonStarted: true,
				Ready:         false,
				State:         "failed",
			},
		},
		{
			name: "rejected",
			start: daemon.StartResult{
				InstanceID:    "instance-rejected",
				Target:        "missing",
				Mode:          api.ModeWatch,
				Accepted:      false,
				DaemonStarted: false,
				Ready:         false,
				State:         "rejected",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := writeJSON(&output, &test.start); err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"instanceId", "target", "mode", "daemonPid", "logPath", "accepted", "daemonStarted", "ready", "state"} {
				if _, ok := payload[field]; !ok {
					t.Fatalf("detached JSON omitted %q: %s", field, output.String())
				}
			}
			if got, ok := payload["accepted"].(bool); !ok || got != test.start.Accepted {
				t.Fatalf("accepted = %#v, want %v", payload["accepted"], test.start.Accepted)
			}
			if got, ok := payload["ready"].(bool); !ok || got != test.start.Ready {
				t.Fatalf("ready = %#v, want %v", payload["ready"], test.start.Ready)
			}
			for _, removed := range []string{"pid", "supervisorStarted", "detached"} {
				if _, exists := payload[removed]; exists {
					t.Fatalf("retired field %s in current start response", removed)
				}
			}
		})
	}
}

func TestRunAcceptsTaskNameAsSyntheticTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "LocalAppData"))
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

func TestPreserveFlushCallErrorAddsStructuredContext(t *testing.T) {
	result := preserveFlushCallError(
		api.FlushResult{},
		fmt.Errorf("daemon transport detail"),
		"/project",
		"instance-1",
		"project-1",
		"up",
		0,
	)
	if result.Worktree != "/project" || result.InstanceID != "instance-1" || result.Project != "project-1" || result.Target != "up" || result.Mode != api.ModeWatch {
		t.Fatalf("flush fallback lost invocation context: %+v", result)
	}
	if result.DurationMs != 1 || result.UpdatedAt.IsZero() {
		t.Fatalf("flush fallback lost timing context: %+v", result)
	}
	if len(result.Issues) != 1 || result.Issues[0].Kind != "daemon_error" || result.Issues[0].Message != "daemon transport detail" {
		t.Fatalf("flush fallback lost daemon error: %+v", result.Issues)
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
	recordCLITestDaemon(t, worktree, api.RunConfig{
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

func TestFlushTimeoutReturnsStructuredFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	recordCLITestDaemon(t, worktree, api.RunConfig{
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

func TestCLIsStatusAndInstallJSON(t *testing.T) {
	marker := filepath.Join(os.TempDir(), "devflow-cli-deps-installed.txt")
	_ = os.Remove(marker)
	bin := filepath.Join(os.TempDir(), cliMissingToolCommand())
	_ = os.Remove(bin)
	t.Setenv("PATH", os.TempDir()+string(os.PathListSeparator)+os.Getenv("PATH"))

	statusOut := &bytes.Buffer{}
	app := &App{Stdout: statusOut, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"clis", "status", "--json", "--project", "cli-deps-project", "--worktree", t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	var statusPayload map[string]any
	if err := json.Unmarshal(statusOut.Bytes(), &statusPayload); err != nil {
		t.Fatal(err)
	}
	requiredCLIs, ok := statusPayload["requiredCLIs"].([]any)
	if !ok || len(requiredCLIs) != 2 {
		t.Fatalf("unexpected required CLIs payload: %+v", statusPayload)
	}

	installOut := &bytes.Buffer{}
	app = &App{Stdout: installOut, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"clis", "install", "--json", "--project", "cli-deps-project", "--worktree", t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected install marker to be written: %v", err)
	}
}

func TestDepsInstallTargetInstallsOnlyMissingRequiredCLIs(t *testing.T) {
	marker := filepath.Join(os.TempDir(), "devflow-cli-deps-installed.txt")
	_ = os.Remove(marker)
	bin := filepath.Join(os.TempDir(), cliMissingToolCommand())
	_ = os.Remove(bin)
	t.Setenv("PATH", os.TempDir()+string(os.PathListSeparator)+os.Getenv("PATH"))

	installOut := &bytes.Buffer{}
	app := &App{Stdout: installOut, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"clis", "install", "--json", "--project", "cli-deps-project", "--target", "installable", "--worktree", t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Target    string   `json:"target"`
		CLIScope  string   `json:"cliScope"`
		Installed []string `json:"installed"`
	}
	if err := json.Unmarshal(installOut.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Target != "installable" || payload.CLIScope != "target" {
		t.Fatalf("unexpected install scope payload: %+v", payload)
	}
	if len(payload.Installed) != 1 || payload.Installed[0] != "missing-tool" {
		t.Fatalf("unexpected installed required CLIs: %+v", payload.Installed)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected install marker to be written: %v", err)
	}
}

func TestDoctorTargetScopesDependencyChecks(t *testing.T) {
	worktree := t.TempDir()

	upOut := &bytes.Buffer{}
	app := &App{Stdout: upOut, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"doctor", "--json", "--project", "cli-target-deps-project", "--target", "up", "--worktree", worktree}); err != nil {
		t.Fatal(err)
	}
	var up api.DoctorResult
	if err := json.Unmarshal(upOut.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if !up.ChecksPassed || up.Target != "up" || up.CLIScope != "target" {
		t.Fatalf("unexpected up doctor result: %+v", up)
	}
	if len(up.RequiredEnv) != 1 || up.RequiredEnv[0].Name != "UP_TOKEN" || !up.RequiredEnv[0].Set || up.RequiredEnv[0].Source != "project" {
		t.Fatalf("unexpected up required env status: %+v", up.RequiredEnv)
	}
	if strings.Contains(strings.Join(up.Warnings, ","), "deploy-tool") || strings.Contains(strings.Join(up.Warnings, ","), "cloud") {
		t.Fatalf("target-scoped doctor reported unrelated dependencies: %+v", up.Warnings)
	}

	deployOut := &bytes.Buffer{}
	app = &App{Stdout: deployOut, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"doctor", "--json", "--project", "cli-target-deps-project", "--target", "deploy", "--worktree", worktree}); err != nil {
		t.Fatal(err)
	}
	var deploy api.DoctorResult
	if err := json.Unmarshal(deployOut.Bytes(), &deploy); err != nil {
		t.Fatal(err)
	}
	if deploy.ChecksPassed {
		t.Fatalf("expected deploy doctor to fail: %+v", deploy)
	}
	warnings := strings.Join(deploy.Warnings, "\n")
	if !strings.Contains(warnings, "cloud") || !strings.Contains(warnings, "deploy-tool") || !strings.Contains(warnings, "DEV_DATABASE_URL") || strings.Contains(warnings, "shell") || strings.Contains(warnings, "UP_TOKEN") {
		t.Fatalf("unexpected deploy warnings: %+v", deploy.Warnings)
	}
	strictOut := &bytes.Buffer{}
	app = &App{Stdout: strictOut, Stderr: &bytes.Buffer{}}
	if strictErr := app.Run([]string{"doctor", "--strict", "--json", "--project", "cli-target-deps-project", "--target", "deploy", "--worktree", worktree}); strictErr == nil || !strings.Contains(strictErr.Error(), "doctor checks failed") {
		t.Fatalf("expected strict doctor to exit nonzero after writing JSON, got %v", strictErr)
	}
	var strictResult api.DoctorResult
	if err := json.Unmarshal(strictOut.Bytes(), &strictResult); err != nil || strictResult.ChecksPassed {
		t.Fatalf("strict doctor did not preserve failure JSON: result=%+v err=%v", strictResult, err)
	}
}

func TestDepsStatusTargetScopesDependencyList(t *testing.T) {
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"clis", "status", "--json", "--project", "cli-target-deps-project", "--target", "up", "--worktree", t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Target       string                      `json:"target"`
		CLIScope     string                      `json:"cliScope"`
		RequiredCLIs []project.RequiredCLIStatus `json:"requiredCLIs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Target != "up" || payload.CLIScope != "target" {
		t.Fatalf("unexpected target scope payload: %+v", payload)
	}
	if len(payload.RequiredCLIs) != 1 || payload.RequiredCLIs[0].Name != "shell" {
		t.Fatalf("unexpected target required CLI list: %+v", payload.RequiredCLIs)
	}
}

func shellQuote(value string) string {
	return "'" + value + "'"
}

func TestDefaultLaunchPlanSelectsFreshDetectedWorktree(t *testing.T) {
	worktree := t.TempDir()
	if err := seedExampleWorktree(worktree); err != nil {
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
	if plan.instanceID == "" {
		t.Fatal("expected canonical worktree instance identity")
	}
}

func TestDefaultLaunchPlanDoesNotReadExecutionState(t *testing.T) {
	worktree := t.TempDir()
	if err := embeddedwebapp.SeedWorktree(worktree); err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Resolve(worktree, filepath.Base(worktree))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(worktree, ".devflow", "state", "instances", inst.ID, "instance.json")
	if err := os.WriteFile(path, []byte(`{"id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	plan, err := app.defaultLaunchPlan(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if plan.projectName != "embedded-web-app" || plan.target != "up" || plan.instanceID != inst.ID {
		t.Fatalf("unexpected project launch selection: %+v", plan)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != `{"id":` {
		t.Fatalf("launch selection changed execution evidence: %q, %v", data, err)
	}
}

func TestInitialStatusWaitRequiresMatchingTargetModeAndNodes(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.SaveStatus(worktree, inst.ID, "", "", map[string]api.NodeStatus{}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		waitForInitialStatus(worktree, inst.ID, "up", api.ModeWatch, 2*time.Second)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("empty stale status should not satisfy initial wait")
	case <-time.After(150 * time.Millisecond):
	}

	if err := instance.SaveStatus(worktree, inst.ID, "up", api.ModeWatch, map[string]api.NodeStatus{
		"app": {Name: "app", Kind: string(project.KindService), State: api.StatePending},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("matching non-empty status did not satisfy initial wait")
	}
}

func TestStatusDoesNotStartDaemon(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.SaveStatus(worktree, inst.ID, "up", api.ModeWatch, map[string]api.NodeStatus{
		"build": {Name: "build", Kind: "once", State: api.StateStopped},
	}); err != nil {
		t.Fatal(err)
	}
	starts := 0
	restore := daemon.SetStartDaemonFuncForTest(func(worktree, instanceID, projectName string) error {
		starts++
		return fmt.Errorf("status should not start daemon")
	})
	defer restore()

	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"status", "--worktree", worktree, "--json"}); err != nil {
		t.Fatal(err)
	}
	if starts != 0 {
		t.Fatalf("expected no daemon starts, got %d", starts)
	}
	var status api.StatusResult
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Daemon != nil {
		t.Fatalf("expected no daemon in direct status, got %+v", status.Daemon)
	}
	if !hasNodeState(status.Nodes, "build", api.StateStopped) {
		t.Fatalf("expected persisted node state in status: %+v", status.Nodes)
	}
}

func TestLogsReadsTUIDiagnostic(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	logPath := instance.LogPath(worktree, inst.ID, "tui")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("session started\npanic details\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"logs", "tui", "--worktree", worktree, "--tail", "1", "--json"}); err != nil {
		t.Fatal(err)
	}
	events := decodeJSONLines(t, stdout.Bytes())
	if len(events) != 1 || events[0]["task"] != "tui" || events[0]["line"] != "panic details" {
		t.Fatalf("unexpected TUI diagnostic log events: %+v", events)
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
	if _, err := os.Stat(localProjectBinaryPathForTest(worktree)); err != nil {
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
	if !strings.Contains(text, "\ngo 1.27.1\n") {
		t.Fatalf("expected generated module to use the supported Go version, got:\n%s", text)
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

func localProjectBinaryPathForTest(worktree string) string {
	return filepath.Join(worktree, ".devflow", "bin", "devflow-local"+localProjectBinarySuffix())
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

func TestLocalBuildSourceLabelAllowsProjectOutsideBootstrapRoot(t *testing.T) {
	repoRoot := filepath.Join("repo", "devflow")
	repoFile := filepath.Join(repoRoot, "pkg", "engine", "engine.go")
	if got := localBuildSourceLabel(repoRoot, repoFile); got != "pkg/engine/engine.go" {
		t.Fatalf("unexpected repo source label %q", got)
	}

	projectFile := filepath.Join("tmp", "external-project", localProjectFile)
	got := localBuildSourceLabel(repoRoot, projectFile)
	if !strings.HasPrefix(got, "external/") || !strings.HasSuffix(got, filepath.ToSlash(projectFile)) {
		t.Fatalf("unexpected external source label %q", got)
	}
}

func TestBootstrapRebuildsWhenLocalProjectChanges(t *testing.T) {
	worktree := t.TempDir()
	projectPath := filepath.Join(worktree, localProjectFile)
	writeLocalProjectFile(t, worktree, localProjectSource("local-rebuild-project", "up"))

	if _, err := runBootstrapCommand(t, worktree, "graph", "list", "--json"); err != nil {
		t.Fatalf("initial bootstrap command failed: %v", err)
	}
	binaryPath := localProjectBinaryPathForTest(worktree)
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
	binaryPath := localProjectBinaryPathForTest(worktree)
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
	binaryPath := localProjectBinaryPathForTest(worktree)
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

func TestEnsureLocalProjectBinarySerializesConcurrentBuilds(t *testing.T) {
	worktree := t.TempDir()
	writeLocalProjectFile(t, worktree, localProjectSource("local-concurrent-project", "up"))
	if err := os.WriteFile(filepath.Join(worktree, "devflow_shared.go"), []byte("package main\n\nconst localConcurrentCompanion = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repoRoot, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	logPath := installFakeBuildGo(t)

	const workers = 8
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			path, err := ensureLocalProjectBinary(context.Background(), repoRoot, worktree)
			if err != nil {
				errs <- err
				return
			}
			want := localProjectBinaryPathForTest(worktree)
			if path != want {
				errs <- fmt.Errorf("unexpected local binary path %q, want %q", path, want)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "start\n"); got != 1 {
		t.Fatalf("expected one local build under concurrent callers, got %d\nlog:\n%s", got, string(data))
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
		path := filepath.Join(dir, "devflow-test-bootstrap"+testExeSuffix())
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
	buildFakeGoCommand(t, dir, "upgrade", exitCode)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEVFLOW_FAKE_GO_ARGS", argsPath)
	return argsPath
}

func installFakeBuildGo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "build.log")
	buildFakeGoCommand(t, dir, "build", 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEVFLOW_FAKE_GO_BUILD_LOG", logPath)
	return logPath
}

func buildFakeGoCommand(t *testing.T, dir, mode string, exitCode int) string {
	t.Helper()
	source := filepath.Join(dir, "fake-go", "main.go")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	code := fmt.Sprintf(`package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const mode = %q
const exitCode = %d

func main() {
	switch mode {
	case "env":
		if len(os.Args) > 1 && os.Args[1] == "env" {
			fmt.Println("")
			fmt.Println(os.Getenv("DEVFLOW_TEST_GOPATH"))
		}
	case "upgrade":
		argsPath := os.Getenv("DEVFLOW_FAKE_GO_ARGS")
		mustWrite(argsPath, strings.Join(os.Args[1:], " ")+"\n")
		mustWrite(argsPath+".goproxy", os.Getenv("GOPROXY")+"\n")
		fmt.Println("fake go output")
		if delay := os.Getenv("DEVFLOW_FAKE_GO_UPGRADE_DELAY"); delay != "" {
			parsed, err := time.ParseDuration(delay)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			time.Sleep(parsed)
		}
		os.Exit(exitCode)
	case "build":
		logPath := os.Getenv("DEVFLOW_FAKE_GO_BUILD_LOG")
		appendFile(logPath, "start\n")
		time.Sleep(200 * time.Millisecond)
		out := ""
		for i := 1; i < len(os.Args)-1; i++ {
			if os.Args[i] == "-o" {
				out = os.Args[i+1]
			}
		}
		if out == "" {
			fmt.Fprintln(os.Stderr, "missing -o output")
			os.Exit(2)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			panic(err)
		}
		mustWrite(out, "fake local binary\n")
		appendFile(logPath, "done\n")
	default:
		fmt.Fprintf(os.Stderr, "unknown fake go mode %%q\n", mode)
		os.Exit(2)
	}
}

func mustWrite(path, value string) {
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		panic(err)
	}
}

func appendFile(path, value string) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	if _, err := file.WriteString(value); err != nil {
		panic(err)
	}
}
`, mode, exitCode)
	if err := os.WriteFile(source, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "go"+testExeSuffix())
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(realGo, "build", "-o", output, source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake go: %v\n%s", err, string(out))
	}
	if mode == "upgrade" {
		// Upgrade removes task artifacts; isolate every platform's user cache.
		cacheHome := t.TempDir()
		t.Setenv("HOME", cacheHome)
		t.Setenv("XDG_CACHE_HOME", cacheHome)
		t.Setenv("LOCALAPPDATA", cacheHome)
	}
	return output
}

func testExeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
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

func TestCacheStatusJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "LocalAppData"))
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

func TestCacheKeyAndPathJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "LocalAppData"))
	worktree := t.TempDir()

	keyOut := &bytes.Buffer{}
	app := &App{Stdout: keyOut, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"cache", "key", "--target", "build", "--json", "--project", "cli-task-target-project", "--worktree", worktree}); err != nil {
		t.Fatal(err)
	}
	var keyResult api.CacheKeyResult
	if err := json.Unmarshal(keyOut.Bytes(), &keyResult); err != nil {
		t.Fatal(err)
	}
	if keyResult.Key == "" || keyResult.Target != "build" || len(keyResult.TaskKeys) != 1 || keyResult.TaskKeys[0].Task != "gen" {
		t.Fatalf("unexpected cache key result: %+v", keyResult)
	}

	pathOut := &bytes.Buffer{}
	app = &App{Stdout: pathOut, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"cache", "path", "--json", "--project", "cli-task-target-project", "--worktree", worktree}); err != nil {
		t.Fatal(err)
	}
	var pathResult api.CachePathResult
	if err := json.Unmarshal(pathOut.Bytes(), &pathResult); err != nil {
		t.Fatal(err)
	}
	if pathResult.CacheRoot != instance.CacheRoot() || pathResult.Namespace != "cli-task-target-project" || !strings.HasPrefix(pathResult.NamespacePath, pathResult.CacheRoot) {
		t.Fatalf("unexpected cache path result: %+v", pathResult)
	}
}

func TestCacheKeyManifestCLIReusesSemanticFingerprintWithoutSecretLeaks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "LocalAppData"))
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("input"), 0o644); err != nil {
		t.Fatal(err)
	}
	counterPath := filepath.Join(t.TempDir(), "fingerprint-calls.txt")
	manifestPath := filepath.Join(t.TempDir(), "devflow-cache-manifest.json")
	const secret = "cli-query-secret"
	t.Setenv("DEVFLOW_MANIFEST_COUNTER_FILE", counterPath)
	t.Setenv("DATABASE_URL", "postgresql://user@db.example/app?password="+secret+"&sslmode=require")

	keyStdout := &bytes.Buffer{}
	keyStderr := &bytes.Buffer{}
	app := &App{Stdout: keyStdout, Stderr: keyStderr}
	if err := app.Run([]string{
		"cache", "key",
		"--target", "build",
		"--manifest-out", manifestPath,
		"--json",
		"--project", "cli-manifest-project",
		"--worktree", worktree,
	}); err != nil {
		t.Fatalf("cache key failed: %v\nstderr=%s", err, keyStderr)
	}
	var keyResult api.CacheKeyResult
	if err := json.Unmarshal(keyStdout.Bytes(), &keyResult); err != nil {
		t.Fatalf("cache key JSON: %v\n%s", err, keyStdout)
	}
	if keyResult.Key == "" || keyResult.ManifestPath != manifestPath {
		t.Fatalf("unexpected cache key manifest result: %+v", keyResult)
	}
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifestData), secret) || strings.Contains(string(manifestData), "postgresql://") || strings.Contains(keyStdout.String(), secret) || strings.Contains(keyStderr.String(), secret) {
		t.Fatalf("secret leaked during cache-key preflight:\nmanifest=%s\nstdout=%s\nstderr=%s", manifestData, keyStdout, keyStderr)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("manifest mode = %03o", info.Mode().Perm())
		}
	}

	runStdout := &bytes.Buffer{}
	runStderr := &bytes.Buffer{}
	app = &App{Stdout: runStdout, Stderr: runStderr}
	if err := app.Run([]string{
		"run", "build",
		"--cache-key-manifest", manifestPath,
		"--ci", "--json",
		"--project", "cli-manifest-project",
		"--worktree", worktree,
	}); err != nil {
		t.Fatalf("manifest-backed run failed: %v\nstdout=%s\nstderr=%s", err, runStdout, runStderr)
	}
	var runResult api.RunResult
	if err := json.Unmarshal(runStdout.Bytes(), &runResult); err != nil {
		t.Fatalf("run JSON: %v\n%s", err, runStdout)
	}
	if runResult.CacheKeyManifest == nil || runResult.CacheKeyManifest.ReusedComponents != 1 || len(runResult.CacheKeyManifest.ReusedTasks) != 1 {
		t.Fatalf("missing cache manifest reuse report: %+v", runResult.CacheKeyManifest)
	}
	counterData, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(counterData), "called") != 1 {
		t.Fatalf("fingerprint callback invocation audit = %q, want exactly once", counterData)
	}
	for surface, value := range map[string]string{
		"manifest":   string(manifestData),
		"key stdout": keyStdout.String(),
		"key stderr": keyStderr.String(),
		"run stdout": runStdout.String(),
		"run stderr": runStderr.String(),
	} {
		if strings.Contains(value, secret) || strings.Contains(value, "postgresql://") {
			t.Fatalf("secret leaked through %s: %s", surface, value)
		}
	}
}

func TestRunWithInvalidCacheKeyManifestReturnsStructuredJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "LocalAppData"))
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("input"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(manifestPath, []byte(`{"password":"must-not-leak"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVFLOW_MANIFEST_COUNTER_FILE", filepath.Join(t.TempDir(), "calls.txt"))
	t.Setenv("DATABASE_URL", "postgresql://user@db.example/app?password=runtime-secret")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: stderr}
	err := app.Run([]string{
		"run", "build",
		"--cache-key-manifest", manifestPath,
		"--ci", "--json",
		"--project", "cli-manifest-project",
		"--worktree", worktree,
	})
	if err == nil {
		t.Fatal("expected invalid cache-key manifest to fail")
	}
	var result api.RunResult
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatalf("invalid-manifest JSON: %v\nstdout=%s\nstderr=%s", decodeErr, stdout, stderr)
	}
	if result.Success || result.CacheKeyManifest == nil || result.CacheKeyManifest.Validated || result.Error == nil || !strings.Contains(result.Error.Message, "cache key manifest rejected") {
		t.Fatalf("manifest rejection was not structured: %+v", result)
	}
	for _, value := range []string{stdout.String(), stderr.String(), err.Error()} {
		if strings.Contains(value, "must-not-leak") || strings.Contains(value, "runtime-secret") {
			t.Fatalf("manifest credential leaked through error surface: %s", value)
		}
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
		Name: cliLoopBinary(t, worktree),
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
	previewOut := &bytes.Buffer{}
	app := &App{Stdout: previewOut, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"stop", "--worktree", worktree, "--task", "svc", "--preview", "--json"}); err != nil {
		t.Fatal(err)
	}
	var preview struct {
		Plan api.LifecyclePlan `json:"plan"`
	}
	if err := json.Unmarshal(previewOut.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if strings.Join(preview.Plan.ProcessesToStop, ",") != "svc" || !instance.ProcessAlive(handle.PID()) {
		t.Fatalf("stop preview changed process state or lost scope: %+v", preview.Plan)
	}
	stdout := &bytes.Buffer{}
	app = &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"stop", "--worktree", worktree, "--task", "svc", "--json"}); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Stopped   []string             `json:"stopped"`
		Lifecycle *api.LifecycleResult `json:"lifecycle"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if strings.Join(payload.Stopped, ",") != "svc" || payload.Lifecycle == nil || !payload.Lifecycle.Success || strings.Join(payload.Lifecycle.Affected, ",") != "svc" {
		t.Fatalf("task-scoped stop JSON did not expose exact result: %+v", payload)
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

func TestStopAllStopsManagedDatabaseContainer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	binDir := filepath.Join(worktree, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	type engineRequest struct {
		method string
		path   string
		query  string
	}
	requests := make(chan engineRequest, 2)
	engineServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests <- engineRequest{method: request.Method, path: request.URL.Path, query: request.URL.RawQuery}
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/devflow-pg-test/json") {
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"State":{"Running":true}}`))
			return
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer engineServer.Close()

	// Keep PATH intentionally free of a docker executable: the daemon must use
	// the Engine API endpoint directly, including on Windows.
	t.Setenv("PATH", binDir)
	t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))
	t.Setenv("DOCKER_HOST", "tcp://"+engineServer.Listener.Addr().String())
	t.Setenv("DOCKER_API_VERSION", "1.55")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	t.Setenv("DOCKER_TLS_VERIFY", "")

	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	inst.DB = api.DBInstance{ContainerName: "devflow-pg-test"}
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}
	if err := instance.SaveStatus(worktree, inst.ID, "dev", api.ModeWatch, map[string]api.NodeStatus{}); err != nil {
		t.Fatal(err)
	}

	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"stop", "--worktree", worktree, "--all", "--json"}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	stopped := stringSetFromJSONList(t, payload["stopped"])
	if !stopped["database"] {
		t.Fatalf("expected managed database in stopped payload: %v", payload["stopped"])
	}
	var stopRequest *engineRequest
	for len(requests) > 0 {
		request := <-requests
		if request.method == http.MethodPost {
			stopRequest = &request
		}
	}
	if stopRequest == nil {
		t.Fatal("expected a Docker Engine stop request")
	}
	if !strings.HasSuffix(stopRequest.path, "/containers/devflow-pg-test/stop") || stopRequest.query != "t=10" {
		t.Fatalf("unexpected Docker Engine stop request: %+v", stopRequest)
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
	if !strings.HasPrefix(status.URLs["backend"], "http://localhost:") || !strings.HasPrefix(status.URLs["frontend"], "http://localhost:") {
		t.Fatalf("expected status URLs to be populated: %+v", status.URLs)
	}
	if status.DB.Password != "" || status.DB.URL != "" {
		t.Fatalf("expected status DB details to be sanitized: %+v", status.DB)
	}
	if len(status.Nodes) == 0 {
		t.Fatal("expected status nodes")
	}
	if !hasNodeState(status.Nodes, "backend_dev", api.StateStopped) {
		t.Fatalf("expected backend_dev stopped after CI run readiness probe: %+v", status.Nodes)
	}
	if !hasNodeState(status.Nodes, "frontend_dev", api.StateStopped) {
		t.Fatalf("expected frontend_dev stopped after CI run readiness probe: %+v", status.Nodes)
	}

	waitForCondition(t, 3*time.Second, func() bool {
		state, err := instance.LoadStatus(worktree, runResult.InstanceID)
		if err != nil {
			return false
		}
		data, err := os.ReadFile(state.Nodes["backend_dev"].LogPath)
		return err == nil && len(data) > 0
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
	daemonCtx, daemonCancel := context.WithCancel(context.Background())
	defer daemonCancel()
	daemonHandle, err := process.Start(daemonCtx, process.CommandSpec{Name: cliLoopBinary(t, worktree), Dir: worktree})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		daemonCancel()
		_ = daemonHandle.Wait()
	}()
	if err := recordCLIRunState(inst, api.RunConfig{
		Project:  "go-next-monorepo",
		Target:   "fullstack",
		Mode:     api.ModeDev,
		Detached: true,
	}, daemonHandle.PID(), filepath.Join(worktree, ".devflow", "logs", runResult.InstanceID, "daemon.log")); err != nil {
		t.Fatal(err)
	}
	// Daemon diagnostics own their directory; task attempts live under run records.
	if err := os.MkdirAll(filepath.Join(worktree, ".devflow", "logs", runResult.InstanceID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".devflow", "logs", runResult.InstanceID, "daemon.log"), []byte("daemon line\n"), 0o644); err != nil {
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
	if status.Daemon == nil || !status.Daemon.Alive {
		t.Fatalf("expected live daemon in status: %+v", status.Daemon)
	}

	daemonLogsStdout := &bytes.Buffer{}
	app = &App{Stdout: daemonLogsStdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"logs", "daemon", "--json", "--worktree", worktree, "--tail", "5"}); err != nil {
		t.Fatal(err)
	}
	daemonLogEvents := decodeJSONLines(t, daemonLogsStdout.Bytes())
	if len(daemonLogEvents) == 0 {
		t.Fatal("expected daemon log events from logs command")
	}
	if got := daemonLogEvents[0]["task"]; got != "daemon" {
		t.Fatalf("unexpected daemon logs task: %v", got)
	}
	if daemonLogEvents[0]["line"] != "daemon line" {
		t.Fatalf("unexpected daemon log line: %q", daemonLogEvents[0]["line"])
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
	if err := app.Run([]string{"doctor", "--json", "--worktree", worktree, "--project", "go-next-monorepo", "--target", "fullstack"}); err != nil {
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
	if !ok {
		t.Fatalf("expected stopped service list in stop payload: %v", stopPayload)
	}
	stoppedSet := map[string]bool{}
	for _, item := range stopped {
		if name, ok := item.(string); ok {
			stoppedSet[name] = true
		}
	}
	for _, name := range []string{"daemon"} {
		if !stoppedSet[name] {
			t.Fatalf("expected %q in stop payload: %v", name, stopPayload)
		}
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

func cliLoopBinary(t *testing.T, worktree string) string {
	t.Helper()
	dir := filepath.Join(worktree, ".devflow", "testhelpers")
	source := filepath.Join(dir, "loop.go")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte(`package main

import "time"

func main() {
	for {
		time.Sleep(time.Hour)
	}
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "loop"+testExeSuffix())
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(realGo, "build", "-o", output, source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build loop helper: %v\n%s", err, string(out))
	}
	return output
}

func stringSetFromJSONList(t *testing.T, value any) map[string]bool {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("expected JSON list, got %T: %v", value, value)
	}
	out := map[string]bool{}
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			t.Fatalf("expected string list item, got %T: %v", item, item)
		}
		out[name] = true
	}
	return out
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

func recordCLITestDaemon(t *testing.T, worktree string, run api.RunConfig) string {
	t.Helper()
	inst, err := instance.Resolve(worktree, filepath.Base(worktree))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client, err := daemon.Dial(worktree)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = client.Call(ctx, daemon.Request{Action: daemon.ActionStop, All: true})
		waitForDaemonDisconnect(client, 3*time.Second)
	})
	logPath := filepath.Join(worktree, ".devflow", "logs", inst.ID, "daemon.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := recordCLIRunState(inst, run, os.Getpid(), logPath); err != nil {
		t.Fatal(err)
	}
	return inst.ID
}

func initRepositoryRepairGitWorktree(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	cacheHome := t.TempDir()
	t.Setenv("HOME", cacheHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(cacheHome, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(cacheHome, "LocalAppData"))
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	t.Setenv("GCM_INTERACTIVE", "Never")

	worktree := t.TempDir()
	for _, dir := range []string{"frontend", filepath.Join("backend", "generated")} {
		if err := os.MkdirAll(filepath.Join(worktree, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		".gitignore":                     ".devflow/\n",
		"frontend/app.txt":               "original frontend\n",
		"backend/generated/model.sql.go": "// original generated Go\n",
		"outside.txt":                    "original outside\n",
	}
	for path, data := range files {
		if err := os.WriteFile(filepath.Join(worktree, filepath.FromSlash(path)), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repairGit(t, worktree, "init")
	repairGit(t, worktree, "config", "core.autocrlf", "false")
	repairGit(t, worktree, "add", "--", ".")
	t.Setenv("GIT_AUTHOR_NAME", "Head Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "head@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Head Committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "committer@example.invalid")
	repairGit(t, worktree, "commit", "-m", "initial")
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(key, "")
	}
	return worktree
}

func runRepositoryRepair(t *testing.T, worktree, target string, extra ...string) (api.RunResult, string, string, error) {
	t.Helper()
	args := []string{
		"run", target,
		"--ci",
		"--json",
		"--commit-changes",
		"--commit-path", "frontend",
		"--commit-path", ":(glob)backend/**/*.sql.go",
		"--commit-message", "bot(ci): automated Devflow formatting and generation",
	}
	args = append(args, extra...)
	args = append(args, "--project", "cli-repair-project", "--worktree", worktree)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: stderr}
	runErr := app.Run(args)
	var result api.RunResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode repository repair JSON: %v\nstdout=%s\nstderr=%s\nrunErr=%v", err, stdout, stderr, runErr)
	}
	return result, stdout.String(), stderr.String(), runErr
}

func repairGit(t *testing.T, worktree string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

func repairGitText(t *testing.T, worktree string, args ...string) string {
	t.Helper()
	return strings.TrimSuffix(strings.TrimSuffix(repairGit(t, worktree, args...), "\n"), "\r")
}

func repairGitLines(t *testing.T, worktree string, args ...string) []string {
	t.Helper()
	text := repairGitText(t, worktree, args...)
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	sort.Strings(lines)
	return lines
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

// Every fixture file uses a .txt transport suffix so the fixture remains in a
// published module archive even though the materialized worktree contains its
// own go.mod. Tests never depend on the repository-relative example tree.
//
//go:embed testdata/go-next-worktree
var goNextWorktreeFixture embed.FS

func seedExampleWorktree(dst string) error {
	const fixtureRoot = "testdata/go-next-worktree"
	return fs.WalkDir(goNextWorktreeFixture, fixtureRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(fixtureRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		materialized := strings.TrimSuffix(rel, ".txt")
		if materialized == "dot-env" {
			materialized = ".env"
		}
		target := filepath.Join(dst, materialized)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := goNextWorktreeFixture.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func recordCLIRunState(inst *api.Instance, run api.RunConfig, pid int, logPath string) error {
	inst.LastRun = run
	if err := instance.Save(inst); err != nil {
		return err
	}
	return instance.RecordDaemon(inst, pid, logPath)
}
