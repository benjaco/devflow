package cache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/benjaco/devflow/pkg/project"
)

func TestRestoreWithOpenOutputPreservesReaderAndCache(t *testing.T) {
	for _, test := range []struct {
		name           string
		outputs        project.Outputs
		cachedArtifact string
	}{
		{
			name:           "file output",
			outputs:        project.Outputs{Files: []string{filepath.Join("generated client", "client.txt")}},
			cachedArtifact: filepath.Join("files", "0"),
		},
		{
			name:           "directory output with open child",
			outputs:        project.Outputs{Dirs: []string{"generated client"}},
			cachedArtifact: filepath.Join("dirs", "0", "client.txt"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			worktree := filepath.Join(t.TempDir(), "worktree with spaces")
			outputDir := filepath.Join(worktree, "generated client")
			outputFile := filepath.Join(outputDir, "client.txt")
			if err := os.MkdirAll(outputDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(outputFile, []byte("cached client\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			store := New(filepath.Join(t.TempDir(), "shared cache with spaces"))
			task := project.Task{Name: "client", Kind: project.KindOnce, Outputs: test.outputs}
			if _, err := store.Snapshot(worktree, task, "key"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(outputFile, []byte("original client\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			// Keep a real reader open throughout publication. Some filesystems
			// allow replacement; others reject the rename until the reader closes.
			reader, err := os.Open(outputFile)
			if err != nil {
				t.Fatal(err)
			}
			readerClosed := false
			t.Cleanup(func() {
				if !readerClosed {
					if err := reader.Close(); err != nil {
						t.Errorf("close output reader: %v", err)
					}
				}
			})

			hit, restoreErr := store.RestoreContext(context.Background(), worktree, task.Name, "key", nil)
			if restoreErr == nil {
				if !hit {
					t.Fatal("existing cache entry was not restored")
				}
				if data, err := os.ReadFile(outputFile); err != nil || string(data) != "cached client\n" {
					t.Fatalf("successful restore did not publish cached content: data=%q err=%v", data, err)
				}
			} else {
				if hit {
					t.Fatalf("failed restore reported a cache hit: %v", restoreErr)
				}
				var renameErr *os.LinkError
				if !errors.As(restoreErr, &renameErr) || renameErr.Op != "rename" {
					t.Fatalf("restore failed outside the output rename: %v", restoreErr)
				}
				movedOutput := outputFile
				if len(test.outputs.Dirs) != 0 {
					movedOutput = outputDir
				}
				if renameErr.Old != movedOutput || filepath.Base(renameErr.New) != "0" || filepath.Base(filepath.Dir(renameErr.New)) != "old" {
					t.Fatalf("rename failure did not come from the output backup move: %v", renameErr)
				}
				if data, err := os.ReadFile(outputFile); err != nil || string(data) != "original client\n" {
					t.Fatalf("failed restore changed original output: data=%q err=%v", data, err)
				}
			}
			t.Logf("restore with open reader: hit=%v error=%v", hit, restoreErr)
			if data, err := io.ReadAll(reader); err != nil || string(data) != "original client\n" {
				t.Fatalf("restore changed the open reader's content: data=%q err=%v", data, err)
			}
			cachedPath := filepath.Join(store.EntryDir(task.Name, "key"), test.cachedArtifact)
			if data, err := os.ReadFile(cachedPath); err != nil || string(data) != "cached client\n" {
				t.Fatalf("restore changed shared cache artifact: data=%q err=%v", data, err)
			}
			if _, exists, err := store.Load(task.Name, "key"); err != nil || !exists {
				t.Fatalf("restore invalidated shared cache entry: exists=%v err=%v", exists, err)
			}
			if paths, err := filepath.Glob(filepath.Join(worktree, ".devflow", "cache-restore-*")); err != nil || len(paths) != 0 {
				t.Fatalf("restore left staging directories: paths=%v err=%v", paths, err)
			}

			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			readerClosed = true
			// The same cache entry must restore after the reader closes.
			// No generator or cache replacement is involved in this retry.
			hit, err = store.RestoreContext(context.Background(), worktree, task.Name, "key", nil)
			if err != nil || !hit {
				t.Fatalf("restore after reader closes: hit=%v err=%v; want cache hit", hit, err)
			}
			if data, err := os.ReadFile(outputFile); err != nil || string(data) != "cached client\n" {
				t.Fatalf("restore after reader closes did not publish cached content: data=%q err=%v", data, err)
			}
			if paths, err := filepath.Glob(filepath.Join(worktree, ".devflow", "cache-restore-*")); err != nil || len(paths) != 0 {
				t.Fatalf("restore after reader closes left staging directories: paths=%v err=%v", paths, err)
			}
			t.Logf("restore after reader closes: hit=%v cached content restored", hit)
		})
	}
}
