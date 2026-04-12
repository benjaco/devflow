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
