package instance

import (
	"os"
	"path/filepath"
	"testing"

	"devflow/pkg/api"
)

func TestResolveSameWorktreeSameInstance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()

	first, err := Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("instance ids differ: %s != %s", first.ID, second.ID)
	}
}

func TestSaveWritesRuntimeEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	inst := &api.Instance{
		ID:       "abc123",
		Label:    "test",
		Worktree: worktree,
		Env:      map[string]string{"FOO": "bar"},
		Ports:    map[string]int{},
	}
	if err := Save(inst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(worktree, ".devflow", "state", "instances", "abc123", "runtime.env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "FOO=bar\n" {
		t.Fatalf("runtime env contents = %q", string(data))
	}
}

func TestRecordDetachedRunPersistsSupervisorAndLastRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	inst, err := Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordDetachedRun(inst, api.RunConfig{
		Project:     "go-next-monorepo",
		Target:      "fullstack",
		Mode:        api.ModeWatch,
		MaxParallel: 2,
		Detached:    true,
	}, 4321, filepath.Join(worktree, "supervisor.log")); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Supervisor.PID != 4321 {
		t.Fatalf("unexpected supervisor pid: %d", loaded.Supervisor.PID)
	}
	if loaded.LastRun.Target != "fullstack" || !loaded.LastRun.Detached {
		t.Fatalf("unexpected last run: %+v", loaded.LastRun)
	}
}

func TestStopSupervisorIgnoresMissingProcess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	inst, err := Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	inst.Supervisor = api.SupervisorRef{PID: 999999}
	if err := Save(inst); err != nil {
		t.Fatal(err)
	}
	if err := StopSupervisor(inst); err != nil {
		t.Fatalf("expected missing supervisor process to be ignored, got %v", err)
	}
	loaded, err := Load(worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Supervisor.PID != 0 {
		t.Fatalf("expected supervisor to be cleared, got %+v", loaded.Supervisor)
	}
}
