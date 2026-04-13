package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "devflow/examples/go-next-monorepo"
	"devflow/pkg/api"
	"devflow/pkg/cache"
	"devflow/pkg/instance"
	"devflow/pkg/process"
	"devflow/pkg/project"
)

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

func TestCacheStatusJSON(t *testing.T) {
	worktree := t.TempDir()
	store := cache.New(filepath.Join(worktree, ".devflow", "cache"))
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
	if err := app.Run([]string{"cache", "status", "--json", "--worktree", worktree}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got := int(payload["count"].(float64)); got != 1 {
		t.Fatalf("unexpected cache count: %d", got)
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
	if !ok || len(stopped) < 2 {
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
