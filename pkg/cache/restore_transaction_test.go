package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/pkg/project"
)

func TestRestoreCancellationDuringLaterInstallRecoversAllOriginals(t *testing.T) {
	worktree := t.TempDir()
	store := New(t.TempDir())
	task := project.Task{Name: "generate", Outputs: project.Outputs{Files: []string{"first", "second"}}}
	for _, name := range task.Outputs.Files {
		if err := os.WriteFile(filepath.Join(worktree, name), []byte("cached "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := store.Snapshot(worktree, task, "key")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range task.Outputs.Files {
		if err := os.WriteFile(filepath.Join(worktree, name), []byte("original "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recovered := 0
	move := func(moveCtx context.Context, source, destination string) error {
		if filepath.Base(filepath.Dir(source)) == "old" {
			if moveCtx.Err() != nil {
				t.Fatalf("forward cancellation prevented recovery: %v", moveCtx.Err())
			}
			recovered++
		} else if moveCtx != ctx {
			t.Fatal("publication lost the caller's cancellation context")
		}
		if filepath.Base(filepath.Dir(source)) == "new" && filepath.Base(source) == "1" {
			// The first output is installed and both originals are backed up.
			// Interrupt the second installation before it changes the filesystem.
			assertFileContent(t, filepath.Join(worktree, "first"), "cached first")
			cancel()
			return ctx.Err()
		}
		return fsutil.MovePathWritable(moveCtx, source, destination)
	}
	hit, err := restoreOutputs(ctx, worktree, store.EntryDir(task.Name, "key"), manifest.Outputs, nil, move)
	if hit || !errors.Is(err, context.Canceled) || recovered != 2 {
		t.Fatalf("canceled publication = hit %v, recovered %d, error %v", hit, recovered, err)
	}
	for i, name := range task.Outputs.Files {
		assertFileContent(t, filepath.Join(worktree, name), "original "+name)
		assertFileContent(t, filepath.Join(store.EntryDir(task.Name, "key"), "files", strconv.Itoa(i)), "cached "+name)
	}
	staging, err := filepath.Glob(filepath.Join(worktree, ".devflow", "cache-restore-*"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("completed recovery left staging: %v, %v", staging, err)
	}
}

func TestRestoreBackupFailureRecoversEarlierOutput(t *testing.T) {
	worktree := t.TempDir()
	store := New(t.TempDir())
	task := project.Task{Name: "generate", Outputs: project.Outputs{Files: []string{"first", "second"}}}
	for _, name := range task.Outputs.Files {
		if err := os.WriteFile(filepath.Join(worktree, name), []byte("cached "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := store.Snapshot(worktree, task, "key")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range task.Outputs.Files {
		if err := os.WriteFile(filepath.Join(worktree, name), []byte("original "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var failedDestination string
	move := func(ctx context.Context, source, destination string) error {
		if source == filepath.Join(worktree, "first") {
			// The first backup is the publication boundary: all cached artifacts
			// must already be staged while every original is still untouched.
			staging := filepath.Dir(filepath.Dir(destination))
			for i, name := range task.Outputs.Files {
				assertFileContent(t, filepath.Join(staging, "new", strconv.Itoa(i)), "cached "+name)
				assertFileContent(t, filepath.Join(worktree, name), "original "+name)
			}
		}
		if source == filepath.Join(worktree, "second") {
			// The earlier output has committed; fail before moving this original.
			assertFileContent(t, filepath.Join(worktree, "first"), "cached first")
			failedDestination = destination
			return os.ErrPermission
		}
		return fsutil.MovePathWritable(ctx, source, destination)
	}
	hit, err := restoreOutputs(context.Background(), worktree, store.EntryDir(task.Name, "key"), manifest.Outputs, nil, move)
	if hit || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("backup failure = hit %v, error %v", hit, err)
	}
	for _, detail := range []string{"cache restore backup", strconv.Quote(filepath.Join(worktree, "second")), strconv.Quote(failedDestination)} {
		if !strings.Contains(err.Error(), detail) {
			t.Errorf("backup diagnostic omitted %q: %v", detail, err)
		}
	}
	for i, name := range task.Outputs.Files {
		assertFileContent(t, filepath.Join(worktree, name), "original "+name)
		assertFileContent(t, filepath.Join(store.EntryDir(task.Name, "key"), "files", strconv.Itoa(i)), "cached "+name)
	}
}

func TestRestoreRollbackFailureRetainsExactOriginalBackup(t *testing.T) {
	worktree := t.TempDir()
	store := New(t.TempDir())
	task := project.Task{Name: "generate", Outputs: project.Outputs{Files: []string{"first", "second"}}}
	for _, name := range task.Outputs.Files {
		if err := os.WriteFile(filepath.Join(worktree, name), []byte("cached "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := store.Snapshot(worktree, task, "key")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range task.Outputs.Files {
		if err := os.WriteFile(filepath.Join(worktree, name), []byte("original "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var backup string
	installFailure := errors.New("synthetic installation failure")
	move := func(ctx context.Context, source, destination string) error {
		if filepath.Base(filepath.Dir(source)) == "new" && filepath.Base(source) == "1" {
			return installFailure
		}
		if filepath.Base(filepath.Dir(source)) == "old" && filepath.Base(source) == "1" {
			backup = source
			return os.ErrPermission
		}
		return fsutil.MovePathWritable(ctx, source, destination)
	}
	hit, err := restoreOutputs(context.Background(), worktree, store.EntryDir(task.Name, "key"), manifest.Outputs, nil, move)
	if hit || !errors.Is(err, installFailure) || !errors.Is(err, os.ErrPermission) || backup == "" {
		t.Fatalf("incomplete rollback lost a cause: hit %v, backup %q, error %v", hit, backup, err)
	}
	for _, detail := range []string{"cache restore install", "cache restore rollback", strconv.Quote(backup), strconv.Quote(filepath.Join(worktree, "second")), "recovery files retained"} {
		if !strings.Contains(err.Error(), detail) {
			t.Errorf("recovery diagnostic omitted %q: %v", detail, err)
		}
	}
	assertFileContent(t, backup, "original second")
	// Recovery continues to the earlier output after the second backup fails.
	assertFileContent(t, filepath.Join(worktree, "first"), "original first")
	if _, err := os.Lstat(filepath.Join(worktree, "second")); !os.IsNotExist(err) {
		t.Fatalf("failed installation/rollback unexpectedly published the second output: %v", err)
	}
	assertFileContent(t, filepath.Join(store.EntryDir(task.Name, "key"), "files", "0"), "cached first")
	assertFileContent(t, filepath.Join(store.EntryDir(task.Name, "key"), "files", "1"), "cached second")
}
