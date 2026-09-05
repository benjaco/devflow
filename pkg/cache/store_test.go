package cache

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/pkg/project"
)

func TestSnapshotRestorePreservesReadOnlyPNPMDirectorySymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires developer-mode symlink support")
	}
	worktree := t.TempDir()
	t.Cleanup(func() { _ = fsutil.RemoveAllWritable(filepath.Join(worktree, "node_modules")) })
	modules := filepath.Join(worktree, "node_modules")
	pkg := filepath.Join(modules, ".pnpm", "pkg@1.0.0", "node_modules", "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "index.js"), []byte("module.exports = 1\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(".pnpm", "pkg@1.0.0", "node_modules", "pkg"), filepath.Join(modules, "pkg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pkg, 0o555); err != nil {
		t.Fatal(err)
	}
	task := project.Task{
		Name:    "install",
		Kind:    project.KindOnce,
		Outputs: project.Outputs{Dirs: []string{"node_modules"}},
	}
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	t.Cleanup(func() { _ = fsutil.RemoveAllWritable(cacheRoot) })
	store := New(cacheRoot)
	if _, err := store.Snapshot(worktree, task, "key"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(modules); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Restore(worktree, task.Name, "key"); err != nil || !ok {
		t.Fatalf("restore ok=%v err=%v", ok, err)
	}
	info, err := os.Lstat(filepath.Join(modules, "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("restored pnpm link was expanded: mode=%s", info.Mode())
	}
	if got, err := os.ReadFile(filepath.Join(modules, "pkg", "index.js")); err != nil || string(got) != "module.exports = 1\n" {
		t.Fatalf("read restored linked package: %q, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(modules, ".pnpm", "pkg@1.0.0", "node_modules", "pkg")); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o555 {
		t.Fatalf("restored read-only package mode = %03o, want 555", got)
	}
}

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

func TestSnapshotClassifiesOutputPaths(t *testing.T) {
	worktree := t.TempDir()
	store := New(filepath.Join(worktree, ".devflow", "cache"))
	task := project.Task{
		Name:    "build",
		Kind:    project.KindOnce,
		Outputs: project.Outputs{Paths: []string{"bin/tool", "dist"}},
	}
	if err := os.MkdirAll(filepath.Join(worktree, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "bin", "tool"), []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "dist", "app.js"), []byte("bundle"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := store.Snapshot(worktree, task, "key")
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot paths use native separators even when declarations use slashes.
	if !reflect.DeepEqual(manifest.Outputs.Files, []string{filepath.Join("bin", "tool")}) {
		t.Fatalf("unexpected files: %+v", manifest.Outputs.Files)
	}
	if !reflect.DeepEqual(manifest.Outputs.Dirs, []string{"dist"}) {
		t.Fatalf("unexpected dirs: %+v", manifest.Outputs.Dirs)
	}
	if err := os.RemoveAll(filepath.Join(worktree, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(worktree, "dist")); err != nil {
		t.Fatal(err)
	}
	ok, err := store.Restore(worktree, "build", "key")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected restore hit")
	}
	assertFileContent(t, filepath.Join(worktree, "bin", "tool"), "binary")
	assertFileContent(t, filepath.Join(worktree, "dist", "app.js"), "bundle")
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

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, string(data), want)
	}
}
