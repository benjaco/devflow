package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjaco/devflow/internal/jsonutil"
	"github.com/benjaco/devflow/pkg/project"
)

func portableTaskNames() []string {
	return []string{
		"shared:generate", "shared_generate", "Build", "build",
		"CON", "con.txt", "AUX", "NUL", "COM1", "LPT9",
		"task.", "task ", "compile:客户端", "café", "cafe\u0301",
		strings.Repeat("long-task-name", 32),
	}
}

func TestCacheTaskDirectoriesArePortableBoundedAndDistinct(t *testing.T) {
	store := NewNamespaced(t.TempDir(), "project")
	seen := make(map[string]string)
	for i, task := range portableTaskNames() {
		t.Run(fmt.Sprintf("name-%02d", i), func(t *testing.T) {
			path := store.EntryDir(task, "key")
			rel, err := filepath.Rel(store.EntriesRoot(), path)
			if err != nil || !filepath.IsLocal(rel) {
				t.Fatalf("entry escapes cache namespace: %q, %v", rel, err)
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) != 2 || parts[1] != "key" {
				t.Fatalf("entry must have exactly a task component and key: %q", rel)
			}
			component := parts[0]
			if component == "" || len(component) > 128 || strings.ContainsAny(component, "<>:\"/\\|?*\x00") || strings.TrimRight(component, ". ") != component {
				t.Errorf("task directory is not a bounded portable component: %q", component)
			}
			stem := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
			switch stem {
			case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
				t.Errorf("task directory uses Windows device name %q", component)
			}
			// The same cache can live on a case-insensitive filesystem.
			folded := strings.ToLower(component)
			if previous, ok := seen[folded]; ok {
				t.Errorf("distinct tasks %q and %q share a filesystem component", previous, task)
			}
			seen[folded] = task
			if got := store.EntryDir(task, "key"); got != path {
				t.Errorf("task directory changed between calls: %q != %q", got, path)
			}
		})
	}
}

func TestCachePortableTaskIdentityRoundTripAndMaintenance(t *testing.T) {
	worktree := t.TempDir()
	store := NewNamespaced(t.TempDir(), "project")
	names := portableTaskNames()
	for i, name := range names {
		task := project.Task{Name: name, Kind: project.KindOnce, Outputs: project.Outputs{Files: []string{"artifact.txt"}}}
		for generation := 1; generation <= 2; generation++ {
			value := fmt.Sprintf("task %d generation %d", i, generation)
			if err := os.WriteFile(filepath.Join(worktree, "artifact.txt"), []byte(value), 0o644); err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("key%d", generation)
			manifest, err := store.Snapshot(worktree, task, key)
			if err != nil {
				t.Fatalf("snapshot task %d generation %d: %v", i, generation, err)
			}
			// GC ordering must not depend on filesystem or wall-clock resolution.
			manifest.CreatedAt = fmt.Sprintf("2026-09-0%dT12:00:00Z", generation)
			if err := jsonutil.WriteFileAtomic(store.manifestPath(name, key), manifest); err != nil {
				t.Fatal(err)
			}
			loaded, ok, err := store.Load(name, key)
			if err != nil || !ok || loaded.Task != name || loaded.Key != key {
				t.Fatalf("load task %d generation %d: %+v, %v, %v", i, generation, loaded, ok, err)
			}
		}
	}
	entries, err := store.List()
	if err != nil || len(entries) != 2*len(names) {
		t.Fatalf("list before GC: %d entries, %v; want %d", len(entries), err, 2*len(names))
	}
	wantNames := make(map[string]int, len(names))
	for _, name := range names {
		wantNames[name] = 2
	}
	for _, entry := range entries {
		if entry.Namespace != "project" || wantNames[entry.Task] == 0 {
			t.Fatalf("list changed task or namespace identity: %+v", entry)
		}
		wantNames[entry.Task]--
	}
	removed, err := store.GC(1)
	if err != nil || removed != len(names) {
		t.Fatalf("GC removed %d, %v; want %d", removed, err, len(names))
	}
	for i, name := range names {
		if _, ok, err := store.Load(name, "key1"); ok || err != nil {
			t.Fatalf("old entry for task %d survived GC: %v, %v", i, ok, err)
		}
		if err := os.WriteFile(filepath.Join(worktree, "artifact.txt"), []byte("changed"), 0o644); err != nil {
			t.Fatal(err)
		}
		if ok, err := store.Restore(worktree, name, "key2"); !ok || err != nil {
			t.Fatalf("restore task %d: %v, %v", i, ok, err)
		}
		data, err := os.ReadFile(filepath.Join(worktree, "artifact.txt"))
		if want := fmt.Sprintf("task %d generation 2", i); err != nil || string(data) != want {
			t.Fatalf("restore task %d: %q, %v; want %q", i, data, err, want)
		}
		if err := store.Invalidate(name); err != nil {
			t.Fatalf("invalidate task %d: %v", i, err)
		}
		entries, err = store.List()
		if err != nil || len(entries) != len(names)-i-1 {
			t.Fatalf("list after invalidating task %d: %d entries, %v", i, len(entries), err)
		}
		if _, ok, err := store.Load(name, "key2"); ok || err != nil {
			t.Fatalf("invalidated task %d survived: %v, %v", i, ok, err)
		}
	}
}

func TestCacheMaintenanceIgnoresMismatchedManifestTaskIdentity(t *testing.T) {
	worktree := t.TempDir()
	store := NewNamespaced(t.TempDir(), "project")
	if err := os.WriteFile(filepath.Join(worktree, "artifact.txt"), []byte("artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ task, key, created string }{
		{"source", "one", "2026-09-01T12:00:00Z"},
		{"source", "forged", "2026-09-03T12:00:00Z"},
		{"target", "one", "2026-09-01T12:00:00Z"},
		{"target", "two", "2026-09-02T12:00:00Z"},
	} {
		task := project.Task{Name: entry.task, Kind: project.KindOnce, Outputs: project.Outputs{Files: []string{"artifact.txt"}}}
		manifest, err := store.Snapshot(worktree, task, entry.key)
		if err != nil {
			t.Fatal(err)
		}
		manifest.CreatedAt = entry.created
		if entry.key == "forged" {
			manifest.Task = "target"
		}
		if err := jsonutil.WriteFileAtomic(store.manifestPath(entry.task, entry.key), manifest); err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range []string{"source", "target"} {
		if _, ok, err := store.Load(task, "forged"); ok || err != nil {
			t.Fatalf("mismatched manifest loaded as %q: %v, %v", task, ok, err)
		}
	}
	entries, err := store.List()
	if err != nil || len(entries) != 3 {
		t.Fatalf("list accepted mismatched manifest: %+v, %v", entries, err)
	}
	removed, err := store.GC(1)
	if err != nil || removed != 1 {
		t.Fatalf("GC used mismatched manifest identity: %d, %v", removed, err)
	}
	for _, entry := range []struct{ task, key string }{{"source", "one"}, {"target", "two"}} {
		if _, ok, err := store.Load(entry.task, entry.key); !ok || err != nil {
			t.Fatalf("GC removed authoritative entry %+v: %v, %v", entry, ok, err)
		}
	}
	if _, err := os.Stat(store.manifestPath("source", "forged")); err != nil {
		t.Fatalf("GC acted on mismatched manifest: %v", err)
	}
}
