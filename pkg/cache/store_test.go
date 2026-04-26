package cache

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/benjaco/devflow/pkg/project"
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

func TestListInvalidateAndGC(t *testing.T) {
	worktree := t.TempDir()
	store := New(filepath.Join(worktree, ".devflow", "cache"))
	task := project.Task{
		Name:    "gen",
		Kind:    project.KindOnce,
		Outputs: project.Outputs{Files: []string{"out.txt"}},
	}
	writeOut := func(value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(worktree, "out.txt"), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeOut("one")
	if _, err := store.Snapshot(worktree, task, "key1"); err != nil {
		t.Fatal(err)
	}
	writeOut("two")
	if _, err := store.Snapshot(worktree, task, "key2"); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	removed, err := store.GC(1)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removed entry, got %d", removed)
	}
	entries, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after gc, got %d", len(entries))
	}
	if err := store.Invalidate("gen"); err != nil {
		t.Fatal(err)
	}
	entries, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty cache after invalidate, got %d", len(entries))
	}
}

func TestNamespacedStoreKeepsEntriesUnderNamespace(t *testing.T) {
	worktree := t.TempDir()
	store := NewNamespaced(filepath.Join(worktree, "cache"), "project/a")
	task := project.Task{
		Name:    "gen",
		Kind:    project.KindOnce,
		Outputs: project.Outputs{Files: []string{"out.txt"}},
	}
	if err := os.WriteFile(filepath.Join(worktree, "out.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Snapshot(worktree, task, "key1"); err != nil {
		t.Fatal(err)
	}
	if got, want := store.EntryDir("gen", "key1"), filepath.Join(worktree, "cache", "entries", "project_a", "gen", "key1"); got != want {
		t.Fatalf("unexpected namespaced entry dir: got %q want %q", got, want)
	}
	plain := New(filepath.Join(worktree, "cache"))
	entries, err := plain.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected plain store not to list namespaced entries, got %d", len(entries))
	}
	namespacedEntries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(namespacedEntries) != 1 || namespacedEntries[0].Namespace != "project_a" {
		t.Fatalf("unexpected namespaced entries: %+v", namespacedEntries)
	}
}

func TestConcurrentSnapshotSameKeyPublishesOneEntry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	store := NewNamespaced(root, "project")
	task := project.Task{
		Name:    "gen",
		Kind:    project.KindOnce,
		Outputs: project.Outputs{Files: []string{"out.txt"}},
	}
	worktrees := []string{t.TempDir(), t.TempDir()}
	for _, worktree := range worktrees {
		if err := os.WriteFile(filepath.Join(worktree, "out.txt"), []byte("same"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(worktrees))
	for _, worktree := range worktrees {
		wg.Add(1)
		go func(worktree string) {
			defer wg.Done()
			_, err := store.Snapshot(worktree, task, "key1")
			errs <- err
		}(worktree)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one published entry, got %+v", entries)
	}
}
