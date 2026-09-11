//go:build windows

package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/benjaco/devflow/pkg/project"
	"golang.org/x/sys/windows"
)

func TestRestoreWindowsDenyDeleteHandlePreservesOutputsAndCache(t *testing.T) {
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
			name:           "directory output with locked child",
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

			path, err := windows.UTF16PtrFromString(outputFile)
			if err != nil {
				t.Fatal(err)
			}
			// Permit reads and writes, but keep rename/delete sharing denied
			// throughout this restore. This is a persistent-lock control.
			handle, err := windows.CreateFile(path, windows.GENERIC_READ,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
				windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
			if err != nil {
				t.Fatal(err)
			}
			handleClosed := false
			t.Cleanup(func() {
				if !handleClosed {
					if err := windows.CloseHandle(handle); err != nil {
						t.Errorf("close deny-delete handle: %v", err)
					}
				}
			})

			hit, restoreErr := store.RestoreContext(context.Background(), worktree, task.Name, "key", nil)
			if hit || restoreErr == nil {
				t.Fatalf("restore with deny-delete handle: hit=%v err=%v; want failed restoration", hit, restoreErr)
			}
			var renameErr *os.LinkError
			if !errors.As(restoreErr, &renameErr) || renameErr.Op != "rename" {
				t.Fatalf("restore failed outside the reported rename path: %v", restoreErr)
			}
			var errno syscall.Errno
			if !errors.As(renameErr.Err, &errno) ||
				(!errors.Is(errno, windows.ERROR_ACCESS_DENIED) &&
					!errors.Is(errno, windows.ERROR_SHARING_VIOLATION) &&
					!errors.Is(errno, windows.ERROR_LOCK_VIOLATION)) {
				t.Fatalf("unexpected native rename error: %v", restoreErr)
			}
			t.Logf("locked restore: hit=%v native errno=%d error=%v", hit, uint32(errno), restoreErr)
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
			cachedPath := filepath.Join(store.EntryDir(task.Name, "key"), test.cachedArtifact)
			if data, err := os.ReadFile(cachedPath); err != nil || string(data) != "cached client\n" {
				t.Fatalf("failed restore changed shared cache artifact: data=%q err=%v", data, err)
			}
			if _, exists, err := store.Load(task.Name, "key"); err != nil || !exists {
				t.Fatalf("failed restore invalidated shared cache entry: exists=%v err=%v", exists, err)
			}
			if paths, err := filepath.Glob(filepath.Join(worktree, ".devflow", "cache-restore-*")); err != nil || len(paths) != 0 {
				t.Fatalf("failed backup left staging directories: paths=%v err=%v", paths, err)
			}

			if err := windows.CloseHandle(handle); err != nil {
				t.Fatal(err)
			}
			handleClosed = true
			// The same cache entry must restore after the fixture releases its
			// handle. No generator or cache replacement is involved in this retry.
			hit, err = store.RestoreContext(context.Background(), worktree, task.Name, "key", nil)
			if err != nil || !hit {
				t.Fatalf("unlocked restore: hit=%v err=%v; want cache hit", hit, err)
			}
			if data, err := os.ReadFile(outputFile); err != nil || string(data) != "cached client\n" {
				t.Fatalf("unlocked restore did not publish cached content: data=%q err=%v", data, err)
			}
			if paths, err := filepath.Glob(filepath.Join(worktree, ".devflow", "cache-restore-*")); err != nil || len(paths) != 0 {
				t.Fatalf("unlocked restore left staging directories: paths=%v err=%v", paths, err)
			}
			t.Logf("unlocked restore: hit=%v cached content restored", hit)
		})
	}
}
