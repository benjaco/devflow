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
