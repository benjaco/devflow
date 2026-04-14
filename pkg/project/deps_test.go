package project

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckDependenciesDetectsInstalledAndMissing(t *testing.T) {
	statuses := CheckDependencies([]Dependency{
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

func TestInstallMissingDependenciesRunsPlatformScriptOnlyForMissingCommands(t *testing.T) {
	workdir := t.TempDir()
	marker := filepath.Join(workdir, "installed.txt")
	binDir := filepath.Join(workdir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fakeCommand := "missing-tool"
	fakePath := filepath.Join(binDir, fakeCommand)
	deps := []Dependency{
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

	result, err := InstallMissingDependencies(context.Background(), workdir, deps, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Installed) != 1 || result.Installed[0] != "missing" {
		t.Fatalf("unexpected installed deps: %+v", result)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "installed\n" {
		t.Fatalf("unexpected install marker contents %q", string(data))
	}
}

func TestInstallMissingDependenciesReportsMissingInstaller(t *testing.T) {
	_, err := InstallMissingDependencies(context.Background(), t.TempDir(), []Dependency{
		{Name: "missing", Command: "definitely-not-installed-command"},
	}, nil)
	if err == nil {
		t.Fatal("expected missing installer error")
	}
}

func TestInstallMissingDependenciesFailsIfCommandStillMissing(t *testing.T) {
	_, err := InstallMissingDependencies(context.Background(), t.TempDir(), []Dependency{
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
