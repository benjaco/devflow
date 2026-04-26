package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunnerBatchesChangedFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.txt")
	if err := os.WriteFile(path, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner, err := New(Options{
		Root:         root,
		Debounce:     40 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	batches, errs, err := runner.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(path, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("watch error: %v", err)
		}
	case batch := <-batches:
		if len(batch.Files) != 1 || batch.Files[0] != "input.txt" {
			t.Fatalf("unexpected batch: %+v", batch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watch batch")
	}
}

func TestRunnerIncludesExplicitPathUnderIgnoredDir(t *testing.T) {
	root := t.TempDir()
	includeDir := filepath.Join(root, ".devflow", "state", "instances", "abc", "flush", "sync")
	if err := os.MkdirAll(includeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner, err := New(Options{
		Root:         root,
		Debounce:     40 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
		IncludePaths: []string{includeDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	batches, errs, err := runner.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(includeDir, "flush-1.sync"), []byte("sync"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("watch error: %v", err)
		}
	case batch := <-batches:
		want := ".devflow/state/instances/abc/flush/sync/flush-1.sync"
		if !containsString(batch.Files, want) {
			t.Fatalf("unexpected batch: %+v", batch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for include-path watch batch")
	}
}

func TestRunnerStillIgnoresOtherDevflowPathsWithIncludePath(t *testing.T) {
	root := t.TempDir()
	includeDir := filepath.Join(root, ".devflow", "state", "instances", "abc", "flush", "sync")
	if err := os.MkdirAll(includeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	otherDir := filepath.Join(root, ".devflow", "logs")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner, err := New(Options{
		Root:         root,
		Debounce:     40 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
		IncludePaths: []string{includeDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	batches, errs, err := runner.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(otherDir, "task.log"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("watch error: %v", err)
		}
	case batch := <-batches:
		t.Fatalf("unexpected ignored .devflow batch: %+v", batch)
	case <-time.After(250 * time.Millisecond):
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
