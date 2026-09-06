package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/daemon"
)

func TestBootstrapJSONErrorsFromParsingThroughExecution(t *testing.T) {
	isolateJSONContractState(t)
	worktree := t.TempDir()
	writeLocalProjectFile(t, worktree, jsonContractProjectSource)
	t.Cleanup(func() { stopJSONContractDaemon(t, worktree) })

	for _, tc := range []struct {
		name  string
		args  []string
		code  string
		phase string
	}{
		{"unknown target", []string{"graph", "show", "missing-target", "--json"}, "unknown_target", "resolution"},
		{"unknown project", []string{"graph", "list", "--project", "missing-project", "--json"}, "unknown_project", "resolution"},
		{"direct unknown target", []string{"run", "missing-target", "--ci", "--json"}, "unknown_target", "resolution"},
		{"direct unknown project", []string{"run", "fail", "--ci", "--project", "missing-project", "--json"}, "unknown_project", "resolution"},
		{"attached unknown target", []string{"run", "missing-target", "--json"}, "unknown_target", "resolution"},
		{"attached unknown project", []string{"run", "fail", "--project", "missing-project", "--json"}, "unknown_project", "resolution"},
		{"JSON before invalid flag", []string{"graph", "list", "--json", "--invalid"}, "invalid_arguments", "parsing"},
		{"JSON after invalid flag", []string{"graph", "list", "--invalid", "--json"}, "invalid_arguments", "parsing"},
		{"explicit JSON boolean", []string{"graph", "list", "--invalid", "--json=true"}, "invalid_arguments", "parsing"},
		{"invalid integer", []string{"run", "fail", "--max-parallel", "bad", "--ci", "--json"}, "invalid_arguments", "parsing"},
		{"invalid validation mode", []string{"validate", "watch", "--mode", "missing-mode", "--json"}, "invalid_arguments", "parsing"},
		{"invalid validation details", []string{"validate", "watch", "--details", "missing-details", "--json"}, "invalid_arguments", "parsing"},
		{"missing positional target", []string{"graph", "show", "--json"}, "invalid_arguments", "parsing"},
		{"unknown subcommand", []string{"graph", "missing-subcommand", "--json"}, "invalid_arguments", "parsing"},
		{"unknown command", []string{"missing-command", "--json"}, "invalid_arguments", "parsing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runJSONContractCommand(t, worktree, tc.args...)
			assertJSONContractFailure(t, stdout, stderr, err, tc.code, tc.phase)
			if strings.TrimSpace(stderr) != "" {
				t.Fatalf("parsing or resolution duplicated its diagnostic on stderr: %q", stderr)
			}
		})
	}

	for _, direct := range []bool{true, false} {
		name := "attached task failure"
		args := []string{"run", "fail", "--json"}
		if direct {
			name = "direct task failure"
			args = append(args, "--ci")
		}
		t.Run(name, func(t *testing.T) {
			stdout, stderr, err := runJSONContractCommand(t, worktree, args...)
			payload := assertJSONContractFailure(t, stdout, stderr, err, "task_failed", "execution")
			var result api.RunResult
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("decode retained run evidence: %v", err)
			}
			if result.Target != "fail" || len(result.Nodes) != 1 || result.Nodes[0].State != api.StateFailed || result.Nodes[0].LogPath == "" {
				t.Fatalf("failed run lost target or node evidence: %s", stdout)
			}
			if !bytes.Contains(payload["failureExcerpts"], []byte("failure-evidence-marker")) {
				t.Fatalf("failed run lost bounded log evidence: %s", stdout)
			}
		})
	}
}

func TestBootstrapJSONModeDoesNotConsumeArgumentValues(t *testing.T) {
	isolateJSONContractState(t)
	worktree := t.TempDir()
	writeLocalProjectFile(t, worktree, jsonContractProjectSource)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"string flag value", []string{"graph", "list", "--project", "--json"}},
		{"equals string flag value", []string{"graph", "list", "--project=--json"}},
		{"after flag terminator", []string{"graph", "show", "--", "--json"}},
		{"explicit false", []string{"graph", "list", "--invalid", "--json=false"}},
		{"last JSON flag wins", []string{"graph", "list", "--json", "--invalid", "--json=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runJSONContractCommand(t, worktree, tc.args...)
			if err == nil {
				t.Fatalf("expected usage or resolution failure, stdout=%q stderr=%q", stdout, stderr)
			}
			if strings.TrimSpace(stdout) != "" || strings.TrimSpace(stderr) == "" {
				t.Fatalf("argument value activated JSON mode: stdout=%q stderr=%q", stdout, stderr)
			}
		})
	}
}

func TestBootstrapJSONRejectsExtraPositionals(t *testing.T) {
	isolateJSONContractState(t)
	worktree := t.TempDir()
	writeLocalProjectFile(t, worktree, jsonContractProjectSource)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"graph target", []string{"graph", "show", "watch", "extra", "--json"}},
		{"run target", []string{"run", "fail", "extra", "--ci", "--json"}},
		{"watch target", []string{"watch", "watch", "extra", "--detach", "--json"}},
		{"log task", []string{"logs", "observe", "extra", "--json"}},
		{"status flags only", []string{"status", "extra", "--json"}},
		{"version flags only", []string{"version", "extra", "--json"}},
		{"graph list flags only", []string{"graph", "list", "extra", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { stopJSONContractDaemon(t, worktree) })
			stdout, stderr, err := runJSONContractCommand(t, worktree, tc.args...)
			assertJSONContractFailure(t, stdout, stderr, err, "invalid_arguments", "parsing")
		})
	}
}

func TestBootstrapJSONErrorsBeforeLocalBinary(t *testing.T) {
	isolateJSONContractState(t)
	for _, tc := range []struct {
		name   string
		source string
		code   string
	}{
		{name: "missing adapter", code: "adapter_not_found"},
		{name: "broken adapter", source: "package main\n\nfunc broken(\n", code: "adapter_compile_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worktree := t.TempDir()
			if tc.source != "" {
				writeLocalProjectFile(t, worktree, tc.source)
			}
			stdout, stderr, err := runJSONContractCommand(t, worktree, "graph", "list", "--json")
			assertJSONContractFailure(t, stdout, stderr, err, tc.code, "bootstrap")
		})
	}
}

func assertJSONContractFailure(t *testing.T, stdout, stderr string, commandErr error, code, phase string) map[string]json.RawMessage {
	t.Helper()
	if commandErr == nil {
		t.Fatalf("expected nonzero exit, stdout=%q stderr=%q", stdout, stderr)
	}
	var payload map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(stdout))
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("expected a JSON error result: %v\nstdout=%q\nstderr=%q", err, stdout, stderr)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("expected exactly one JSON result, next decode=%v value=%v\nstdout=%q", err, extra, stdout)
	}
	if value, ok := payload["success"]; !ok || !bytes.Equal(bytes.TrimSpace(value), []byte("false")) {
		t.Fatalf("error result must include success:false: %s", stdout)
	}
	var detail struct {
		Code    string `json:"code"`
		Phase   string `json:"phase"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload["error"], &detail); err != nil {
		t.Fatalf("expected structured error: %v\nstdout=%q", err, stdout)
	}
	if detail.Code != code || detail.Phase != phase || strings.TrimSpace(detail.Message) == "" {
		t.Fatalf("error=%+v, want code=%q phase=%q and a message", detail, code, phase)
	}
	if _, exists := payload["code"]; exists {
		t.Fatalf("error code must have one location: %s", stdout)
	}
	return payload
}

func runJSONContractCommand(t *testing.T, worktree string, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, buildBootstrapBinary(t), args...)
	cmd.Dir = worktree
	cmd.Env = withEnv(os.Environ(), envBootstrapEntry, "1")
	cmd.Env = withEnv(cmd.Env, envBootstrapRoot, root)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("command %q exceeded deadline: stdout=%q stderr=%q", args, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String(), err
}

func isolateJSONContractState(t *testing.T) {
	t.Helper()
	// Keep normal Go build caches while isolating Devflow's registry and task
	// state on every supported OS, including LOCALAPPDATA on Windows.
	output, err := exec.Command("go", "env", "-json", "GOCACHE", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	var goEnv map[string]string
	if err := json.Unmarshal(output, &goEnv); err != nil {
		t.Fatal(err)
	}
	for key, value := range goEnv {
		t.Setenv(key, value)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "LocalAppData"))
}

func stopJSONContractDaemon(t *testing.T, worktree string) {
	t.Helper()
	client, err := daemon.Dial(worktree)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		return
	}
	if _, err := client.Call(ctx, daemon.Request{Action: daemon.ActionStop, All: true}); err != nil {
		t.Errorf("stop fixture daemon: %v", err)
		return
	}
	waitForDaemonDisconnect(client, 3*time.Second)
}

const jsonContractProjectSource = `package main

import (
	"context"
	"errors"

	"github.com/benjaco/devflow/pkg/project"
)

type jsonContractProject struct{}

func init() { project.Register(jsonContractProject{}) }
func (jsonContractProject) Name() string { return "json-contract-project" }
func (jsonContractProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "json-contract"}, nil
}
func (jsonContractProject) Tasks() []project.Task {
	return []project.Task{
		{Name: "failure", Kind: project.KindOnce, Run: func(_ context.Context, rt *project.Runtime) error {
			rt.EmitLogLine("stderr", "ERROR failure-evidence-marker")
			return errors.New("deliberate task failure")
		}},
		{Name: "observe", Kind: project.KindOnce, Inputs: project.Inputs{Files: []string{"source.txt"}}, Run: func(_ context.Context, rt *project.Runtime) error {
			rt.EmitLogLine("stdout", "watch-evidence-marker")
			return nil
		}},
	}
}
func (jsonContractProject) Targets() []project.Target {
	return []project.Target{
		{Name: "fail", RootTasks: []string{"failure"}},
		{Name: "watch", RootTasks: []string{"observe"}},
	}
}
`
