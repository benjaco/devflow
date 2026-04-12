package cache

import (
	"os"
	"path/filepath"
	"testing"

	"devflow/pkg/project"
)

func TestSnapshotRestoreReplacesDeclaredOutputsOnly(t *testing.T) {
	worktree := t.TempDir()
	store := New(filepath.Join(worktree, ".devflow", "cache"))
	task := project.Task{
		Name:    "gen",
		Kind:    project.KindOnce,
		Outputs: project.Outputs{Files: []string{"out.txt"}},
	}

	if err := os.WriteFile(filepath.Join(worktree, "out.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Snapshot(worktree, task, "key1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "out.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Restore(worktree, "gen", "key1"); err != nil || !ok {
		t.Fatalf("restore ok=%v err=%v", ok, err)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "v1" {
		t.Fatalf("restored output=%q want v1", string(data))
	}
	keep, err := os.ReadFile(filepath.Join(worktree, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(keep) != "keep" {
		t.Fatalf("undeclared file changed: %q", string(keep))
	}
}

func TestCorruptManifestIsTreatedAsMiss(t *testing.T) {
	worktree := t.TempDir()
	store := New(filepath.Join(worktree, ".devflow", "cache"))
	entry := store.EntryDir("gen", "broken")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "manifest.json"), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ok, err := store.Load("gen", "broken")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected corrupt manifest to be treated as cache miss")
	}
}
