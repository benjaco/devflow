package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

func TestProjectVersionHandoffAcceptsFutureCommand(t *testing.T) {
	t.Setenv(envBootstrapRoot, "")
	t.Setenv(envBootstrapEntry, "")
	t.Setenv(envLocalExec, "")
	t.Setenv(envBootstrapModuleVersion, "")
	t.Setenv(envProjectRuntime, "")
	t.Setenv(envLauncherVersion, "")
	t.Setenv("GOWORK", "off")
	t.Setenv("DEVFLOW_TEST_PROJECT_VERSION_VALUE", "invocation value")
	worktree := t.TempDir()
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "cmd", "devflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "go.mod"), "module github.com/benjaco/devflow\n\ngo 1.27.1\n")
	writeTestFile(t, filepath.Join(source, "cmd", "devflow", "main.go"), fmt.Sprintf(projectVersionCommandSource, "selected future runtime"))
	writeTestFile(t, filepath.Join(worktree, "go.mod"), fmt.Sprintf("module example.com/project\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v0.1.0\n\nreplace github.com/benjaco/devflow => %q\n", filepath.ToSlash(source)))
	writeTestFile(t, filepath.Join(worktree, "go.sum"), "")

	args := []string{"future-command", "--future-mode", "new value", "--json"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, buildBootstrapBinary(t), args...)
	cmd.Dir = worktree
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("project-selected command failed: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	var result struct {
		Marker string   `json:"marker"`
		Args   []string `json:"args"`
		Value  string   `json:"value"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("selected command must emit one JSON result: %v\n%s", err, &stdout)
	}
	if result.Marker != "selected future runtime" || result.Value != "invocation value" || !reflect.DeepEqual(result.Args, args) {
		t.Fatalf("project handoff changed command/environment: %+v; want args=%q", result, args)
	}
}

func TestProjectVersionHandoffRoutesInvocations(t *testing.T) {
	isolateJSONContractState(t)
	t.Setenv(envBootstrapRoot, "")
	t.Setenv(envBootstrapEntry, "")
	t.Setenv(envLocalExec, "")
	t.Setenv(envBootstrapModuleVersion, "")
	t.Setenv(envProjectRuntime, "")
	t.Setenv(envLauncherVersion, "")
	t.Setenv("GOWORK", "off")
	worktree, source, otherDirectory := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "cmd", "devflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "go.mod"), "module github.com/benjaco/devflow\n\ngo 1.27.1\n")
	writeTestFile(t, filepath.Join(source, "cmd", "devflow", "main.go"), fmt.Sprintf(projectVersionCommandSource, "project owns parsing"))
	writeTestFile(t, filepath.Join(worktree, "go.mod"), fmt.Sprintf("module example.com/project\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v0.1.0\n\nreplace github.com/benjaco/devflow => %q\n", filepath.ToSlash(source)))
	writeTestFile(t, filepath.Join(worktree, "go.sum"), "")
	writeTestFile(t, filepath.Join(otherDirectory, "go.mod"), "invalid unrelated module\n")
	// Only metadata is needed to route historical/recovery commands by instance;
	// this fixture starts no engine, daemon or service.
	inst, err := instance.Resolve(worktree, "selected project")
	if err != nil {
		t.Fatal(err)
	}
	instancePath := filepath.Join(worktree, ".devflow", "state", "instances", inst.ID, "instance.json")
	instanceBefore, err := os.ReadFile(instancePath)
	if err != nil {
		t.Fatal(err)
	}
	launcher := buildBootstrapBinary(t)
	for _, test := range []struct {
		name string
		dir  string
		args []string
	}{
		{name: "bare devflow", dir: worktree, args: []string{}},
		{name: "new flag on known command", dir: worktree, args: []string{"run", "verify", "--future-option", "new value", "--ci", "--json"}},
		{name: "worktree after unknown flag", dir: otherDirectory, args: []string{"run", "verify", "--future-option", "new value", "--worktree", worktree, "--ci", "--json"}},
		{name: "future command with explicit worktree", dir: otherDirectory, args: []string{"future-command", "--future-option", "--worktree=" + worktree, "--json"}},
		{name: "worktree token is a known option value", dir: worktree, args: []string{"run", "--project", "--worktree", "--ci", "--json"}},
		{name: "json token is a known option value", dir: worktree, args: []string{"graph", "list", "--project", "--json"}},
		{name: "flag terminator preserves literal tokens", dir: worktree, args: []string{"future-command", "--", "--worktree", otherDirectory, "--json"}},
		{name: "version without an adapter", dir: worktree, args: []string{"version", "--json"}},
		{name: "logs without an adapter", dir: worktree, args: []string{"logs", "check", "--json"}},
		{name: "runs without an adapter", dir: worktree, args: []string{"runs", "list", "--json"}},
		{name: "prompts without an adapter", dir: worktree, args: []string{"prompts", "list", "--json"}},
		{name: "instance selects recovery project from another directory", dir: otherDirectory, args: []string{"logs", "check", "--instance", inst.ID, "--json"}},
		{name: "instance after unknown flag selects recovery project", dir: otherDirectory, args: []string{"logs", "check", "--future-option", "--instance", inst.ID, "--json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, launcher, test.args...)
			cmd.Dir = test.dir
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("handoff failed: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
			}
			var result struct {
				Marker string   `json:"marker"`
				Args   []string `json:"args"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("handoff stdout is not one JSON result: %v\n%s", err, &stdout)
			}
			if result.Marker != "project owns parsing" || !reflect.DeepEqual(result.Args, test.args) {
				t.Fatalf("selected runtime did not receive unchanged arguments: %+v; want %q", result, test.args)
			}
		})
	}
	if data, err := os.ReadFile(instancePath); err != nil || !bytes.Equal(data, instanceBefore) {
		t.Fatalf("routing changed persisted instance metadata: %q; %v", data, err)
	}
}

func TestProjectVersionHandoffCachesAndRefreshesSelectedSource(t *testing.T) {
	t.Setenv(envBootstrapRoot, "")
	t.Setenv(envBootstrapEntry, "")
	t.Setenv(envLocalExec, "")
	t.Setenv(envBootstrapModuleVersion, "")
	t.Setenv(envProjectRuntime, "")
	t.Setenv(envLauncherVersion, "")
	t.Setenv("GOWORK", "off")
	worktree, source := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "cmd", "devflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "go.mod"), "module github.com/benjaco/devflow\n\ngo 1.27.1\n")
	sourcePath := filepath.Join(source, "cmd", "devflow", "main.go")
	writeTestFile(t, sourcePath, fmt.Sprintf(projectVersionCommandSource, "first version"))
	modulePath := filepath.Join(worktree, "go.mod")
	module := fmt.Sprintf("module example.com/project\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v0.1.0\n\nreplace github.com/benjaco/devflow => %q\n", filepath.ToSlash(source))
	writeTestFile(t, modulePath, module)
	writeTestFile(t, filepath.Join(worktree, "go.sum"), "")
	launcher := buildBootstrapBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, launcher, "future-command", "--json")
	cmd.Dir = worktree
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("first selected runtime: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	var first struct {
		Marker     string `json:"marker"`
		Executable string `json:"executable"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &first); err != nil || first.Marker != "first version" {
		t.Fatalf("first runtime result: %+v; decode=%v; stdout=%s", first, err, &stdout)
	}
	firstFile, err := os.Stat(first.Executable)
	if err != nil {
		t.Fatal(err)
	}

	// A prepared runtime must work without invoking Go again.
	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	stdout.Reset()
	stderr.Reset()
	cmd = exec.CommandContext(ctx, launcher, "future-command", "--json")
	cmd.Dir, cmd.Stdout, cmd.Stderr = worktree, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("cached runtime required Go: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	var cached struct {
		Marker     string `json:"marker"`
		Executable string `json:"executable"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &cached); err != nil || cached.Marker != "first version" || cached.Executable != first.Executable {
		t.Fatalf("cached result changed: %+v; decode=%v; stdout=%s", cached, err, &stdout)
	}
	cachedFile, err := os.Stat(cached.Executable)
	if err != nil || !os.SameFile(firstFile, cachedFile) {
		t.Fatalf("cached invocation replaced the prepared executable: %v", err)
	}
	t.Setenv("PATH", originalPath)

	writeTestFile(t, sourcePath, fmt.Sprintf(projectVersionCommandSource, "edited source"))
	stdout.Reset()
	stderr.Reset()
	cmd = exec.CommandContext(ctx, launcher, "future-command", "--json")
	cmd.Dir, cmd.Stdout, cmd.Stderr = worktree, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("edited selected source: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	var edited struct {
		Marker     string `json:"marker"`
		Executable string `json:"executable"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &edited); err != nil || edited.Marker != "edited source" || edited.Executable == first.Executable {
		t.Fatalf("source edit reused stale runtime: %+v; decode=%v; stdout=%s", edited, err, &stdout)
	}
	if data, err := os.ReadFile(modulePath); err != nil || string(data) != module {
		t.Fatalf("runtime preparation changed project go.mod: %q; %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "go.sum")); err != nil || len(data) != 0 {
		t.Fatalf("runtime preparation changed project go.sum: %q; %v", data, err)
	}

	module = strings.Replace(module, "v0.1.0", "v0.2.0", 1)
	writeTestFile(t, modulePath, module)
	stdout.Reset()
	stderr.Reset()
	cmd = exec.CommandContext(ctx, launcher, "future-command", "--json")
	cmd.Dir, cmd.Stdout, cmd.Stderr = worktree, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("updated project pin: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	var repinned struct {
		Marker     string `json:"marker"`
		Executable string `json:"executable"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &repinned); err != nil || repinned.Marker != "edited source" || repinned.Executable == edited.Executable {
		t.Fatalf("pin edit reused stale selection: %+v; decode=%v; stdout=%s", repinned, err, &stdout)
	}

	writeTestFile(t, sourcePath, "package main\nfunc main(\n")
	stdout.Reset()
	stderr.Reset()
	cmd = exec.CommandContext(ctx, launcher, "future-command", "--json")
	cmd.Dir, cmd.Stdout, cmd.Stderr = worktree, &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("invalid new source ran old runtime successfully: stdout=%s stderr=%s", &stdout, &stderr)
	}
	var failed struct {
		Marker  string `json:"marker"`
		Success bool   `json:"success"`
		Error   *struct {
			Code  string `json:"code"`
			Phase string `json:"phase"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &failed); err != nil || failed.Marker != "" || failed.Success || failed.Error == nil || failed.Error.Phase != "bootstrap" {
		t.Fatalf("failed preparation did not return one bootstrap error: %+v; decode=%v; stdout=%s", failed, err, &stdout)
	}
	if _, err := os.Stat(repinned.Executable); err != nil {
		t.Fatalf("failed rebuild removed the previous prepared binary: %v", err)
	}
	if data, err := os.ReadFile(modulePath); err != nil || string(data) != module {
		t.Fatalf("updated pin was rewritten: %q; %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "go.sum")); err != nil || len(data) != 0 {
		t.Fatalf("updated runtime changed go.sum: %q; %v", data, err)
	}
}

func TestProjectVersionHandoffRunsRealAdapterAndReportsVersion(t *testing.T) {
	isolateJSONContractState(t)
	t.Setenv(envBootstrapRoot, "")
	t.Setenv(envBootstrapEntry, "")
	t.Setenv(envLocalExec, "")
	t.Setenv(envBootstrapModuleVersion, "")
	t.Setenv(envProjectRuntime, "")
	t.Setenv(envLauncherVersion, "")
	t.Setenv("GOWORK", "off")
	source, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	module := fmt.Sprintf("module example.com/pinned-project\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v0.1.0\n\nreplace github.com/benjaco/devflow => %q\n", filepath.ToSlash(source))
	writeTestFile(t, filepath.Join(worktree, "go.mod"), module)
	writeTestFile(t, filepath.Join(worktree, "go.sum"), "")
	writeTestFile(t, filepath.Join(worktree, localProjectFile), localProjectSource("pinned-real-project", "verify"))
	launcher := buildBootstrapBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, launcher, "version", "--json")
	cmd.Dir = t.TempDir()
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("unpinned launcher version: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	var launcherVersion api.VersionResult
	if err := json.Unmarshal(stdout.Bytes(), &launcherVersion); err != nil || launcherVersion.Version == "" || launcherVersion.ProjectVersion != "" {
		t.Fatalf("unpinned launcher version result: %+v; decode=%v; stdout=%s", launcherVersion, err, &stdout)
	}
	stdout.Reset()
	stderr.Reset()
	cmd = exec.CommandContext(ctx, launcher, "graph", "list", "--json")
	cmd.Dir, cmd.Stdout, cmd.Stderr = worktree, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("pinned adapter graph: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	var graph struct {
		Targets []string `json:"targets"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &graph); err != nil || len(graph.Targets) != 1 || graph.Targets[0] != "verify" {
		t.Fatalf("selected adapter graph result: %+v; decode=%v; stdout=%s", graph, err, &stdout)
	}
	stdout.Reset()
	stderr.Reset()
	cmd = exec.CommandContext(ctx, launcher, "run", "verify", "--ci", "--json")
	cmd.Dir, cmd.Stdout, cmd.Stderr = worktree, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("pinned adapter run: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	var run api.RunResult
	if err := json.Unmarshal(stdout.Bytes(), &run); err != nil || !run.Success || run.Mode != api.ModeCI || run.Target != "verify" {
		t.Fatalf("selected adapter run result: %+v; decode=%v; stdout=%s", run, err, &stdout)
	}
	if len(run.Nodes) != 1 || run.Nodes[0].Name != "noop" || run.Nodes[0].State != api.StateDone {
		t.Fatalf("selected runtime did not execute the adapter task: %+v", run.Nodes)
	}
	otherDirectory := t.TempDir()
	writeTestFile(t, filepath.Join(otherDirectory, "go.mod"), "invalid unrelated module\n")
	writeTestFile(t, filepath.Join(otherDirectory, localProjectFile), "package main\nfunc invalid(\n")
	stdout.Reset()
	stderr.Reset()
	cmd = exec.CommandContext(ctx, launcher, "status", "--instance", run.InstanceID, "--json")
	cmd.Dir, cmd.Stdout, cmd.Stderr = otherDirectory, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("instance-selected status read the unrelated adapter: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	var status api.StatusResult
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil || status.InstanceID != run.InstanceID || status.Target != "verify" || len(status.Nodes) != 1 || status.Nodes[0].Name != "noop" || status.Nodes[0].State != api.StateDone {
		t.Fatalf("instance-selected status lost run evidence: %+v; decode=%v; stdout=%s", status, err, &stdout)
	}

	// Version inspection must work even when the adapter cannot compile.
	writeTestFile(t, filepath.Join(worktree, localProjectFile), "package main\nfunc invalid(\n")
	stdout.Reset()
	stderr.Reset()
	cmd = exec.CommandContext(ctx, launcher, "version", "--json")
	cmd.Dir, cmd.Stdout, cmd.Stderr = worktree, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("version required a working adapter: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	var version api.VersionResult
	if err := json.Unmarshal(stdout.Bytes(), &version); err != nil || version.Version != "devel" || version.ModulePath != "github.com/benjaco/devflow" {
		t.Fatalf("source replacement must identify development code honestly: %+v; decode=%v; stdout=%s", version, err, &stdout)
	}
	if version.ProjectVersion != "v0.1.0" || version.LauncherVersion != launcherVersion.Version || version.VCSRevision != "" {
		t.Fatalf("version must distinguish the project pin, source code and launcher: %+v", version)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "go.mod")); err != nil || string(data) != module {
		t.Fatalf("adapter bootstrap rewrote project go.mod: %q; %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "go.sum")); err != nil || len(data) != 0 {
		t.Fatalf("adapter bootstrap rewrote project go.sum: %q; %v", data, err)
	}

	// An explicit source-development override wins even when the project pin
	// points at a checkout that is unavailable on this machine.
	module = fmt.Sprintf("module example.com/pinned-project\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v0.1.0\n\nreplace github.com/benjaco/devflow => %q\n", filepath.ToSlash(filepath.Join(worktree, "unavailable-devflow")))
	writeTestFile(t, filepath.Join(worktree, "go.mod"), module)
	writeTestFile(t, filepath.Join(worktree, localProjectFile), localProjectSource("pinned-real-project", "verify"))
	t.Setenv(envBootstrapRoot, source)
	stdout.Reset()
	stderr.Reset()
	cmd = exec.CommandContext(ctx, launcher, "graph", "list", "--json")
	cmd.Dir, cmd.Stdout, cmd.Stderr = worktree, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("project pin defeated explicit source development: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	if err := json.Unmarshal(stdout.Bytes(), &graph); err != nil || len(graph.Targets) != 1 || graph.Targets[0] != "verify" {
		t.Fatalf("source override did not load the adapter: %+v; decode=%v; stdout=%s", graph, err, &stdout)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "go.mod")); err != nil || string(data) != module {
		t.Fatalf("source override rewrote project go.mod: %q; %v", data, err)
	}
}

func TestProjectVersionPreparationHonorsProgress(t *testing.T) {
	t.Setenv(envBootstrapRoot, "")
	t.Setenv(envBootstrapEntry, "")
	t.Setenv(envLocalExec, "")
	t.Setenv(envBootstrapModuleVersion, "")
	t.Setenv(envProjectRuntime, "")
	t.Setenv(envLauncherVersion, "")
	t.Setenv("GOWORK", "off")
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "cmd", "devflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "go.mod"), "module github.com/benjaco/devflow\n\ngo 1.27.1\n")
	writeTestFile(t, filepath.Join(source, "cmd", "devflow", "main.go"), fmt.Sprintf(projectVersionCommandSource, "prepared runtime"))
	launcher := buildBootstrapBinary(t)
	for _, progress := range []string{"quiet", "states"} {
		t.Run(progress, func(t *testing.T) {
			worktree := t.TempDir()
			writeTestFile(t, filepath.Join(worktree, "go.mod"), fmt.Sprintf("module example.com/project\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v0.1.0\n\nreplace github.com/benjaco/devflow => %q\n", filepath.ToSlash(source)))
			args := []string{"run", "verify", "--ci", "--progress", progress, "--json"}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, launcher, args...)
			cmd.Dir = worktree
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("preparation failed: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
			}
			var result struct {
				Marker string   `json:"marker"`
				Args   []string `json:"args"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Marker != "prepared runtime" || !reflect.DeepEqual(result.Args, args) {
				t.Fatalf("preparation contaminated final JSON or arguments: %+v; decode=%v; stdout=%s", result, err, &stdout)
			}
			if progress == "quiet" && stderr.Len() != 0 {
				t.Fatalf("quiet preparation printed progress: %s", &stderr)
			}
			if progress == "states" && !strings.Contains(stderr.String(), "[devflow] preparing project version v0.1.0") {
				t.Fatalf("states preparation omitted lifecycle progress: %s", &stderr)
			}
		})
	}
}

func TestProjectVersionUpgradeBypassesInvalidModule(t *testing.T) {
	t.Setenv(envBootstrapRoot, "")
	t.Setenv(envBootstrapEntry, "")
	t.Setenv(envLocalExec, "")
	t.Setenv(envBootstrapModuleVersion, "")
	t.Setenv(envProjectRuntime, "")
	t.Setenv(envLauncherVersion, "")
	worktree := t.TempDir()
	writeTestFile(t, filepath.Join(worktree, "go.mod"), "invalid module declaration\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// An invalid option reaches the installed upgrade parser without installing
	// anything; reading the broken project module would return a different error.
	cmd := exec.CommandContext(ctx, buildBootstrapBinary(t), "upgrade", "--unknown-option", "--json")
	cmd.Dir = worktree
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("upgrade unexpectedly accepted invalid option: stdout=%s stderr=%s", &stdout, &stderr)
	}
	var result struct {
		Error *struct {
			Code  string `json:"code"`
			Phase string `json:"phase"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Error == nil || result.Error.Code != "invalid_arguments" || result.Error.Phase != "parsing" {
		t.Fatalf("project module intercepted upgrade: %+v; decode=%v; stdout=%s", result, err, &stdout)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".devflow")); !os.IsNotExist(err) {
		t.Fatalf("upgrade argument parsing prepared a project runtime: %v", err)
	}
}

func TestProjectVersionNestedGlobalInvocationSelectsItsOwnProject(t *testing.T) {
	t.Setenv(envBootstrapRoot, "")
	t.Setenv(envBootstrapEntry, "")
	t.Setenv(envLocalExec, "")
	t.Setenv(envBootstrapModuleVersion, "")
	t.Setenv(envProjectRuntime, "")
	t.Setenv(envLauncherVersion, "")
	t.Setenv("GOWORK", "off")
	firstWorktree, secondWorktree := t.TempDir(), t.TempDir()
	firstSource, secondSource := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(firstSource, "cmd", "devflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(secondSource, "cmd", "devflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(firstSource, "go.mod"), "module github.com/benjaco/devflow\n\ngo 1.27.1\n")
	writeTestFile(t, filepath.Join(secondSource, "go.mod"), "module github.com/benjaco/devflow\n\ngo 1.27.1\n")
	writeTestFile(t, filepath.Join(firstSource, "cmd", "devflow", "main.go"), `package main

import (
	"os"
	"os/exec"
)

func main() {
	executable, err := os.Executable()
	if err != nil { panic(err) }
	child := exec.Command(os.Getenv("DEVFLOW_TEST_PROJECT_VERSION_LAUNCHER"), "future-command", "--json")
	child.Dir = os.Getenv("DEVFLOW_TEST_PROJECT_VERSION_CHILD_WORKTREE")
	// Reproduce a task's inherited bootstrap markers, including an execution
	// guard belonging to the parent executable rather than the new launcher.
	child.Env = append(os.Environ(), "DEVFLOW_LOCAL_EXEC="+executable)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Run(); err != nil { os.Exit(1) }
}
`)
	writeTestFile(t, filepath.Join(secondSource, "cmd", "devflow", "main.go"), fmt.Sprintf(projectVersionCommandSource, "second project's runtime"))
	writeTestFile(t, filepath.Join(firstWorktree, "go.mod"), fmt.Sprintf("module example.com/first\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v0.1.0\n\nreplace github.com/benjaco/devflow => %q\n", filepath.ToSlash(firstSource)))
	writeTestFile(t, filepath.Join(secondWorktree, "go.mod"), fmt.Sprintf("module example.com/second\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v0.2.0\n\nreplace github.com/benjaco/devflow => %q\n", filepath.ToSlash(secondSource)))
	launcher := buildBootstrapBinary(t)
	t.Setenv("DEVFLOW_TEST_PROJECT_VERSION_LAUNCHER", launcher)
	t.Setenv("DEVFLOW_TEST_PROJECT_VERSION_CHILD_WORKTREE", secondWorktree)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, launcher, "future-command", "--json")
	cmd.Dir = firstWorktree
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("nested global invocation inherited the first project's runtime: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	var result struct {
		Marker string   `json:"marker"`
		Args   []string `json:"args"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Marker != "second project's runtime" || !reflect.DeepEqual(result.Args, []string{"future-command", "--json"}) {
		t.Fatalf("nested invocation lost its project selection: %+v; decode=%v; stdout=%s", result, err, &stdout)
	}
}

const projectVersionCommandSource = `package main

import (
	"encoding/json"
	"os"
)

func main() {
	executable, err := os.Executable()
	if err != nil { panic(err) }
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
		"marker": %q,
		"args": os.Args[1:],
		"value": os.Getenv("DEVFLOW_TEST_PROJECT_VERSION_VALUE"),
		"executable": executable,
	}); err != nil { panic(err) }
}
`
