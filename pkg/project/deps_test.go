package project

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type cliScopeProject struct{}

func (cliScopeProject) Name() string { return "dependency-scope" }

func (cliScopeProject) ConfigureInstance(ctx context.Context, worktree string) (InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return InstanceConfig{}, nil
}

func (cliScopeProject) Tasks() []Task {
	return []Task{
		{Name: "codegen", Kind: KindOnce, RequiredCLIs: []string{"sqlc"}},
		{Name: "build", Kind: KindOnce, Deps: []string{"codegen"}, RequiredCLIs: []string{"go"}},
		{Name: "e2e", Kind: KindOnce, RequiredCLIs: []string{"playwright"}},
	}
}

func (cliScopeProject) Targets() []Target {
	return []Target{
		{Name: "up", RootTasks: []string{"build"}, RequiredCLIs: []string{"docker"}},
		{Name: "e2e", RootTasks: []string{"e2e"}},
	}
}

func (cliScopeProject) RequiredCLIs() []RequiredCLI {
	return []RequiredCLI{
		{Name: "docker", Command: "docker"},
		{Name: "go", Command: "go"},
		{Name: "playwright", Command: "playwright"},
		{Name: "sqlc", Command: "sqlc"},
	}
}

func TestCheckRequiredCLIsDetectsInstalledAndMissing(t *testing.T) {
	statuses := CheckRequiredCLIs([]RequiredCLI{
		{Name: "shell", Command: "sh"},
		{Name: "missing", Command: "definitely-not-installed-command"},
	})
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}
	if !statuses[0].Installed {
		t.Fatalf("expected %q to be installed", statuses[0].Name)
	}
	if statuses[1].Installed {
		t.Fatalf("expected %q to be missing", statuses[1].Name)
	}
}

func TestRequiredCLIsForTargetUsesOnlyTargetClosure(t *testing.T) {
	clis, err := RequiredCLIsForTarget(cliScopeProject{}, "up")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(clis))
	for _, cli := range clis {
		names = append(names, cli.Name)
	}
	if strings.Join(names, ",") != "docker,go,sqlc" {
		t.Fatalf("unexpected target CLIs: %v", names)
	}
}

func TestRequiredCLIsForTargetAcceptsCommandAliases(t *testing.T) {
	p := cliScopeProjectWithTasks([]Task{
		{Name: "build", Kind: KindOnce, RequiredCLIs: []string{"go"}},
	})
	clis, err := RequiredCLIsForTarget(p, "up")
	if err != nil {
		t.Fatal(err)
	}
	if len(clis) != 1 || clis[0].Name != "golang" {
		t.Fatalf("unexpected target CLIs: %+v", clis)
	}
}

func TestRequiredCLIsForTargetRejectsUnknownCLI(t *testing.T) {
	p := cliScopeProjectWithTasks([]Task{
		{Name: "build", Kind: KindOnce, RequiredCLIs: []string{"unknown"}},
	})
	_, err := RequiredCLIsForTarget(p, "up")
	if err == nil || !strings.Contains(err.Error(), `task "build": unknown required CLI "unknown"`) {
		t.Fatalf("expected unknown required CLI error, got %v", err)
	}
}

func TestInstallMissingRequiredCLIsRunsPlatformScriptOnlyForMissingCommands(t *testing.T) {
	workdir := t.TempDir()
	marker := filepath.Join(workdir, "installed.txt")
	binDir := filepath.Join(workdir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fakeCommand := "missing-tool"
	fakePath := filepath.Join(binDir, fakeCommand)
	clis := []RequiredCLI{
		{
			Name:    "shell",
			Command: "sh",
			Install: map[string]InstallScript{
				runtime.GOOS: {Script: "echo should-not-run >> " + shellQuote(marker)},
			},
		},
		{
			Name:    "missing",
			Command: fakeCommand,
			Install: map[string]InstallScript{
				runtime.GOOS: {Script: strings.Join([]string{
					"echo installed >> " + shellQuote(marker),
					"cat > " + shellQuote(fakePath) + " <<'EOF'",
					"#!/bin/sh",
					"exit 0",
					"EOF",
					"chmod +x " + shellQuote(fakePath),
				}, "\n")},
			},
		},
	}

	result, err := InstallMissingRequiredCLIs(context.Background(), workdir, clis, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Installed) != 1 || result.Installed[0] != "missing" {
		t.Fatalf("unexpected installed CLIs: %+v", result)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "installed\n" {
		t.Fatalf("unexpected install marker contents %q", string(data))
	}
}

func TestInstallMissingRequiredCLIsReportsMissingInstaller(t *testing.T) {
	_, err := InstallMissingRequiredCLIs(context.Background(), t.TempDir(), []RequiredCLI{
		{Name: "missing", Command: "definitely-not-installed-command"},
	}, nil)
	if err == nil {
		t.Fatal("expected missing installer error")
	}
}

func TestInstallMissingRequiredCLIsFailsIfCommandStillMissing(t *testing.T) {
	_, err := InstallMissingRequiredCLIs(context.Background(), t.TempDir(), []RequiredCLI{
		{
			Name:    "missing",
			Command: "still-not-installed-command",
			Install: map[string]InstallScript{runtime.GOOS: {Script: "echo noop"}},
		},
	}, nil)
	if err == nil {
		t.Fatal("expected install verification error")
	}
}

func shellQuote(value string) string {
	return "'" + value + "'"
}

type cliScopeProjectWithTasks []Task

func (p cliScopeProjectWithTasks) Name() string { return "dependency-scope-custom" }

func (p cliScopeProjectWithTasks) ConfigureInstance(ctx context.Context, worktree string) (InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return InstanceConfig{}, nil
}

func (p cliScopeProjectWithTasks) Tasks() []Task { return []Task(p) }

func (p cliScopeProjectWithTasks) Targets() []Target {
	return []Target{{Name: "up", RootTasks: []string{"build"}}}
}

func (p cliScopeProjectWithTasks) RequiredCLIs() []RequiredCLI {
	return []RequiredCLI{
		{Name: "golang", Command: "go"},
		{Name: "sqlc", Command: "sqlc"},
	}
}
