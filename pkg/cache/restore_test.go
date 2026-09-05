package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/internal/jsonutil"
	"github.com/benjaco/devflow/pkg/project"
)

func restoreFixture(t *testing.T) (string, *Store, project.Task) {
	t.Helper()
	worktree := t.TempDir()
	store := New(t.TempDir())
	task := project.Task{Name: "build", Kind: project.KindOnce, Outputs: project.Outputs{Files: []string{"first.txt", "second.txt"}}}
	for _, rel := range task.Outputs.Files {
		if err := os.WriteFile(filepath.Join(worktree, rel), []byte("cached "+rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Snapshot(worktree, task, "key"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range task.Outputs.Files {
		if err := os.WriteFile(filepath.Join(worktree, rel), []byte("current "+rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return worktree, store, task
}

func assertCurrentOutputs(t *testing.T, worktree string, task project.Task) {
	t.Helper()
	for _, rel := range task.Outputs.Files {
		assertFileContent(t, filepath.Join(worktree, rel), "current "+rel)
	}
	staging, err := filepath.Glob(filepath.Join(worktree, ".devflow", "cache-restore-*"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("restore left staging directories: %v, %v", staging, err)
	}
}

func TestRestoreMissingArtifactPreservesAllOutputs(t *testing.T) {
	worktree, store, task := restoreFixture(t)
	if err := os.Remove(filepath.Join(store.EntryDir(task.Name, "key"), "files", "1")); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Restore(worktree, task.Name, "key"); ok || err != nil {
		t.Fatalf("corrupt cache restore = %v, %v; want miss", ok, err)
	}
	assertCurrentOutputs(t, worktree, task)
	if _, ok, err := store.Load(task.Name, "key"); ok || err != nil {
		t.Fatalf("corrupt entry was not invalidated: %v, %v", ok, err)
	}
}

func TestRestoreCancellationPreservesOutputsAndSharedCache(t *testing.T) {
	for _, duringCopy := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-copy", true: "during-copy"}[duringCopy], func(t *testing.T) {
			worktree, store, task := restoreFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if !duringCopy {
				cancel()
			}
			ok, err := store.RestoreContext(ctx, worktree, task.Name, "key", func(progress fsutil.CopyProgress) {
				if progress.Done {
					cancel()
				}
			})
			if ok || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled restore = %v, %v", ok, err)
			}
			assertCurrentOutputs(t, worktree, task)
			if _, ok, err := store.Load(task.Name, "key"); !ok || err != nil {
				t.Fatalf("cancellation damaged shared cache: %v, %v", ok, err)
			}
		})
	}
}

func TestRestoreRollsBackEarlierOutputsOnCommitFailure(t *testing.T) {
	worktree, store, task := restoreFixture(t)
	completed := 0
	ok, err := store.RestoreContext(context.Background(), worktree, task.Name, "key", func(progress fsutil.CopyProgress) {
		if !progress.Done {
			return
		}
		completed++
		if completed != 2 {
			return
		}
		// Remove a staged artifact after its successful copy. Publication of
		// the first output then succeeds; the second fails after backing up
		// its current output, requiring both originals to be rolled back.
		paths, err := filepath.Glob(filepath.Join(worktree, ".devflow", "cache-restore-*", "new", "1"))
		if err != nil || len(paths) != 1 {
			t.Fatalf("find staged artifact: %v, %v", paths, err)
		}
		if err := os.Remove(paths[0]); err != nil {
			t.Fatal(err)
		}
	})
	if ok || err == nil {
		t.Fatalf("failed commit = %v, %v; want error", ok, err)
	}
	assertCurrentOutputs(t, worktree, task)
	if ok, err := store.Restore(worktree, task.Name, "key"); !ok || err != nil {
		t.Fatalf("retry from intact shared cache = %v, %v", ok, err)
	}
}

func TestRestoreRejectsInvalidManifestWithoutChangingOutputs(t *testing.T) {
	for _, mutate := range []struct {
		name string
		fn   func(*Manifest)
	}{
		{"version", func(m *Manifest) { m.Version = 2 }},
		{"task", func(m *Manifest) { m.Task = "other" }},
		{"key", func(m *Manifest) { m.Key = "other" }},
		{"empty-outputs", func(m *Manifest) { m.Outputs = Outputs{} }},
		{"parent-traversal", func(m *Manifest) { m.Outputs.Files[0] = "../outside.txt" }},
		{"windows-traversal", func(m *Manifest) { m.Outputs.Files[0] = `..\outside.txt` }},
		{"absolute", func(m *Manifest) { m.Outputs.Files[0] = filepath.Join(t.TempDir(), "outside.txt") }},
		{"worktree-root", func(m *Manifest) { m.Outputs.Dirs = []string{"."} }},
		{"git-metadata", func(m *Manifest) { m.Outputs.Files[0] = ".git/config" }},
		{"runtime-root", func(m *Manifest) { m.Outputs.Dirs = []string{".devflow"} }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			worktree, store, task := restoreFixture(t)
			manifest, ok, err := store.Load(task.Name, "key")
			if !ok || err != nil {
				t.Fatalf("load fixture: %v, %v", ok, err)
			}
			mutate.fn(manifest)
			if err := jsonutil.WriteFileAtomic(store.manifestPath(task.Name, "key"), manifest); err != nil {
				t.Fatal(err)
			}
			if ok, err := store.Restore(worktree, task.Name, "key"); ok || err != nil {
				t.Fatalf("invalid manifest restore = %v, %v; want miss", ok, err)
			}
			assertCurrentOutputs(t, worktree, task)
		})
	}
}

func TestCacheRejectsEscapingTaskAndKeyNames(t *testing.T) {
	store := New(t.TempDir())
	for _, value := range []string{"", ".", "..", "../outside", "nested/name", `nested\name`, `C:\outside`} {
		if _, _, err := store.Load(value, "key"); err == nil {
			t.Errorf("accepted task %q", value)
		}
		if _, _, err := store.Load("task", value); err == nil {
			t.Errorf("accepted key %q", value)
		}
		if value != "" {
			if err := store.Invalidate(value); err == nil {
				t.Errorf("accepted invalidation task %q", value)
			}
		}
	}
}

func TestCachePreservesHostValidColonNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("colons are not valid Windows path components")
	}
	worktree := t.TempDir()
	store := New(t.TempDir())
	if err := os.WriteFile(filepath.Join(worktree, "result:client"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := project.Task{Name: "build:client", Outputs: project.Outputs{Files: []string{"result:client"}}}
	if _, err := store.Snapshot(worktree, task, "key"); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Restore(worktree, task.Name, "key"); !ok || err != nil {
		t.Fatalf("restore host-valid colon names: %v, %v", ok, err)
	}
}

func TestSnapshotRejectsIncorrectOutputKinds(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(worktree, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, outputs := range []project.Outputs{{Files: []string{"dir"}}, {Dirs: []string{"file"}}} {
		store := New(t.TempDir())
		task := project.Task{Name: "build", Outputs: outputs}
		if _, err := store.Snapshot(worktree, task, "key"); err == nil {
			t.Fatalf("accepted incorrect output kind: %+v", outputs)
		}
		if _, ok, err := store.Load(task.Name, "key"); ok || err != nil {
			t.Fatalf("invalid snapshot was published: %v, %v", ok, err)
		}
	}
}

func TestSnapshotNormalizesRedundantOutputs(t *testing.T) {
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(worktree, "dist", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "dist", "sub", "out"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := New(t.TempDir())
	task := project.Task{Name: "build", Outputs: project.Outputs{Paths: []string{"dist", "dist/sub/out"}, Files: []string{"dist/sub/out"}, Dirs: []string{"dist/sub", "dist"}}}
	manifest, err := store.Snapshot(worktree, task, "key")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Outputs.Files) != 0 || !reflect.DeepEqual(manifest.Outputs.Dirs, []string{"dist"}) {
		t.Fatalf("redundant outputs not normalized: %+v", manifest.Outputs)
	}
	if err := os.RemoveAll(filepath.Join(worktree, "dist")); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Restore(worktree, task.Name, "key"); !ok || err != nil {
		t.Fatalf("restore normalized output: %v, %v", ok, err)
	}
	assertFileContent(t, filepath.Join(worktree, "dist", "sub", "out"), "cached")
}

func TestCacheRejectsSymlinkAncestors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires developer-mode symlink support")
	}
	worktree := t.TempDir()
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "out"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(worktree, "linked")); err != nil {
		t.Fatal(err)
	}
	store := New(t.TempDir())
	task := project.Task{Name: "build", Outputs: project.Outputs{Files: []string{"linked/out"}}}
	if _, err := store.Snapshot(worktree, task, "key"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("snapshot followed an output ancestor: %v", err)
	}
	manifest := Manifest{Version: 1, Task: task.Name, Key: "key", Outputs: Outputs{Files: task.Outputs.Files}}
	if err := jsonutil.WriteFileAtomic(store.manifestPath(task.Name, "key"), manifest); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Restore(worktree, task.Name, "key"); ok || err == nil {
		t.Fatalf("restore followed an output ancestor: %v, %v", ok, err)
	}
	assertFileContent(t, filepath.Join(external, "out"), "outside")
}

func TestRestoreLegacyRedundantOutputsRetainsManifestIndexes(t *testing.T) {
	worktree := t.TempDir()
	store := New(t.TempDir())
	entry := store.EntryDir("build", "key")
	if err := os.MkdirAll(filepath.Join(entry, "dirs", "1", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "dirs", "1", "sub", "out"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Version: 1, Task: "build", Key: "key", Outputs: Outputs{
		Files: []string{"dist/sub/out", "dist/sub/out"}, Dirs: []string{"dist/sub", "dist", "dist"},
	}}
	if err := jsonutil.WriteFileAtomic(store.manifestPath("build", "key"), manifest); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Restore(worktree, "build", "key"); !ok || err != nil {
		t.Fatalf("restore legacy overlapping outputs: %v, %v", ok, err)
	}
	assertFileContent(t, filepath.Join(worktree, "dist", "sub", "out"), "cached")
}

func TestRestoreCorruptNestedSymlinkPreservesOutputs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires developer-mode symlink support")
	}
	worktree := t.TempDir()
	store := New(t.TempDir())
	dir := filepath.Join(worktree, "dist")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := project.Task{Name: "build", Outputs: project.Outputs{Dirs: []string{"dist"}}}
	if _, err := store.Snapshot(worktree, task, "key"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out"), []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := store.EntryDir(task.Name, "key")
	if err := os.WriteFile(filepath.Join(entry, "dirs", "outside"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(entry, "dirs", "0", "bad-link")); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Restore(worktree, task.Name, "key"); ok || err != nil {
		t.Fatalf("corrupt linked artifact restore = %v, %v; want miss", ok, err)
	}
	assertFileContent(t, filepath.Join(dir, "out"), "current")
	if _, ok, err := store.Load(task.Name, "key"); ok || err != nil {
		t.Fatalf("corrupt linked entry remains: %v, %v", ok, err)
	}
}
