package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"golang.org/x/mod/modfile"
)

func TestProjectVersionErrorPrecedesCommandParsing(t *testing.T) {
	t.Setenv(envBootstrapRoot, "")
	t.Setenv(envLocalExec, "")
	worktree := t.TempDir()
	t.Chdir(worktree)
	if err := os.WriteFile(filepath.Join(worktree, "go.mod"), []byte("module example.com/app\nrequire github.com/benjaco/devflow not-a-version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (&App{Stdout: &stdout, Stderr: &stderr}).Run([]string{"future-command", "--json"})
	var result struct{ Error api.CommandError }
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatalf("invalid JSON: %v; stdout=%s stderr=%s", decodeErr, &stdout, &stderr)
	}
	if err == nil || result.Error.Code != "invalid_project_version" || result.Error.Phase != "bootstrap" {
		t.Fatalf("project pin was not checked before old command parsing: err=%v result=%s", err, &stdout)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".devflow")); !os.IsNotExist(err) {
		t.Fatalf("invalid pin created runtime state: %v", err)
	}
}

func TestInvocationFlagFindsWorktreeWithoutParsingFutureFlags(t *testing.T) {
	known := flag.NewFlagSet("run", flag.ContinueOnError)
	known.String("project", "", "")
	known.String("worktree", "", "")
	known.Bool("ci", false, "")
	for _, test := range []struct {
		name  string
		args  []string
		want  string
		found bool
	}{
		{"after unknown flag", []string{"run", "verify", "--new-option", "new value", "--worktree", "other"}, "other", true},
		{"equals form", []string{"future-command", "--worktree=other"}, "other", true},
		{"known option value", []string{"run", "--project", "--worktree", "other"}, "", false},
		{"after terminator", []string{"run", "--", "--worktree", "other"}, "", false},
		{"last value wins", []string{"run", "--worktree=first", "--worktree", "last"}, "last", true},
		{"missing value", []string{"run", "--worktree"}, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, found := invocationFlag(test.args, known, "worktree", true)
			if got != test.want || found != test.found {
				t.Fatalf("worktree=%q present=%t; want %q present=%t", got, found, test.want, test.found)
			}
		})
	}
}

func TestLocalAdapterModuleUsesProjectPinAndReplacement(t *testing.T) {
	t.Setenv(envBootstrapModuleVersion, "v0.1.0")
	worktree := t.TempDir()
	module := "module example.com/app\nrequire github.com/benjaco/devflow v0.2.0\nreplace github.com/benjaco/devflow v0.2.0 => example.com/devflow v0.3.0\n"
	if err := os.WriteFile(filepath.Join(worktree, "go.mod"), []byte(module), 0o600); err != nil {
		t.Fatal(err)
	}
	generated, err := localBuildModuleSource(filepath.Join(worktree, ".devflow", "localbuild", "test"), "", worktree)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(generated, "require github.com/benjaco/devflow v0.2.0") || !strings.Contains(generated, "replace github.com/benjaco/devflow v0.2.0 => example.com/devflow v0.3.0") || strings.Contains(generated, "v0.1.0") {
		t.Fatalf("adapter module did not preserve the project version: %s", generated)
	}
}

func TestLocalAdapterBuildKeyTracksModuleAndChecksumChanges(t *testing.T) {
	worktree := t.TempDir()
	adapter := filepath.Join(worktree, "devflow.project.go")
	if err := os.WriteFile(adapter, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	modulePath := filepath.Join(worktree, "go.mod")
	if err := os.WriteFile(modulePath, []byte("module example.com/app\nrequire github.com/benjaco/devflow v0.1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := localBuildKey("", adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modulePath, []byte("module example.com/app\nrequire github.com/benjaco/devflow v0.2.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repinned, err := localBuildKey("", adapter)
	if err != nil || before == repinned {
		t.Fatalf("pin change did not invalidate adapter: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "go.sum"), []byte("new checksums\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newSums, err := localBuildKey("", adapter)
	if err != nil || repinned == newSums {
		t.Fatalf("checksum change did not invalidate adapter: %v", err)
	}
	if err := os.Remove(filepath.Join(worktree, "go.sum")); err != nil {
		t.Fatal(err)
	}
	removedSums, err := localBuildKey("", adapter)
	if err != nil || removedSums != repinned {
		t.Fatalf("removed checksums did not restore prior input identity: %v", err)
	}
}

func TestProjectPreparationProgressCannotIssueWorkflowCommands(t *testing.T) {
	var output bytes.Buffer
	writer := projectVersionProgress{out: &output}
	for _, chunk := range []string{":", ":error::oops\n#", "#[group]child\n::endgroup::\n"} {
		if n, err := writer.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("progress write=%d, %v", n, err)
		}
	}
	if strings.Contains(output.String(), "::") || strings.Contains(output.String(), "##[") || !strings.Contains(output.String(), "oops") {
		t.Fatalf("compiler output can issue a workflow command or lost its message: %q", output.String())
	}
}

func TestProjectVersionPreparationErrorKeepsCompactJSON(t *testing.T) {
	t.Setenv(envBootstrapRoot, "")
	t.Setenv(envLocalExec, "")
	worktree := t.TempDir()
	module := "module example.com/app\nrequire github.com/benjaco/devflow " + strings.Repeat("x", 4000) + "\n"
	if err := os.WriteFile(filepath.Join(worktree, "go.mod"), []byte(module), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (&App{Stdout: &stdout, Stderr: &stderr}).Run([]string{"run", "verify", "--worktree", worktree, "--ci", "--details", "summary", "--progress", "quiet", "--json"})
	var result struct {
		Details   string                  `json:"details"`
		Error     api.CommandError        `json:"error"`
		Truncated api.ExecutionTruncation `json:"truncated"`
	}
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatalf("invalid JSON: %v; %s", decodeErr, &stdout)
	}
	if err == nil || result.Details != "summary" || !result.Truncated.Text || len(result.Error.Message) > 2200 || stderr.Len() != 0 {
		t.Fatalf("preparation error lost compact/quiet output: details=%q truncated=%+v messageBytes=%d err=%v stderr=%s", result.Details, result.Truncated, len(result.Error.Message), err != nil, &stderr)
	}
}

func TestLocalAdapterModuleQuotesSourceReplacement(t *testing.T) {
	worktree := t.TempDir()
	sourceRoot := filepath.Join(t.TempDir(), "source checkout")
	generated, err := localBuildModuleSource(filepath.Join(worktree, ".devflow", "localbuild", "test"), sourceRoot, worktree)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := modfile.Parse("generated/go.mod", []byte(generated), nil)
	if err != nil {
		t.Fatalf("source path produced invalid Go module: %v\n%s", err, generated)
	}
	if len(parsed.Replace) != 1 || filepath.FromSlash(parsed.Replace[0].New.Path) != sourceRoot {
		t.Fatalf("generated module changed replacement path: %s", generated)
	}
}

func TestLocalProjectExecutionGuardMatchesExecutable(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "devflow.project.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envLocalExec, executable)
	if shouldExecLocalProject(nil, worktree) {
		t.Fatal("the already-loaded executable must not bootstrap itself again")
	}
	other := filepath.Join(t.TempDir(), "parent-binary")
	if err := os.WriteFile(other, []byte("parent executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envLocalExec, other)
	if !shouldExecLocalProject(nil, worktree) {
		t.Fatal("an inherited guard belonging to another executable suppressed bootstrap")
	}
}
