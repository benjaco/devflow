package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

func TestScanEntryHandlesConcurrentDisappearance(t *testing.T) {
	runner, err := New(Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"walk", "info"} {
		// Windows aliases syscall.ENOTDIR to a not-found error. Use portable
		// categories here; real non-directory ancestors are tested below.
		for _, test := range []struct {
			name string
			err  error
			want error
		}{
			{"disappeared", os.ErrNotExist, nil},
			{"permission", os.ErrPermission, os.ErrPermission},
			{"invalid", os.ErrInvalid, os.ErrInvalid},
		} {
			t.Run(phase+"/"+test.name, func(t *testing.T) {
				path := filepath.Join(runner.root, "child.txt")
				pathErr := &os.PathError{Op: phase, Path: path, Err: test.err}
				var entry os.DirEntry = scanInfoErrorEntry{err: pathErr}
				var walkErr error
				if phase == "walk" {
					entry, walkErr = nil, pathErr
				}
				current := map[string]fileState{}
				if err := runner.scanEntry(context.Background(), path, entry, walkErr, current); !errors.Is(err, test.want) {
					t.Fatalf("scan error = %v, want %v", err, test.want)
				}
				if test.want == nil {
					before := map[string]fileState{"child.txt": {mode: 0o600, size: 1}}
					if got := changedFiles(before, current); !reflect.DeepEqual(got, []string{"child.txt"}) {
						t.Fatalf("disappeared child did not become an input change: %v", got)
					}
				}
			})
		}
	}
}

func TestRunnerRejectsInputUnderNonDirectoryAncestor(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "blocked"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner, err := New(Options{Root: root, WatchOnly: true, WatchPaths: []string{"blocked/missing/input.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := checkMissingScanRoot(ctx, filepath.Join(root, "blocked", "input.txt")); err == nil {
		t.Fatal("missing-root classification accepted a regular-file parent")
	}
	if _, _, err := runner.Start(ctx); err == nil {
		t.Fatal("watch accepted an unobservable input below a regular file")
	}
}

func TestRunnerAllowsMissingInputAncestors(t *testing.T) {
	runner, err := New(Options{Root: t.TempDir(), WatchOnly: true, WatchPaths: []string{"missing/parent/input.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := runner.Start(ctx); err != nil {
		t.Fatalf("watch rejected a not-yet-created input: %v", err)
	}
}

type scanInfoErrorEntry struct {
	err error
}

func (scanInfoErrorEntry) Name() string                 { return "child.txt" }
func (scanInfoErrorEntry) IsDir() bool                  { return false }
func (scanInfoErrorEntry) Type() os.FileMode            { return 0 }
func (e scanInfoErrorEntry) Info() (os.FileInfo, error) { return nil, e.err }

func TestDirectoryChildChangesDoNotReportUnchangedParent(t *testing.T) {
	before := map[string]fileState{
		"src": {mode: os.ModeDir | 0o755, isDir: true, modTime: time.Unix(1, 0), size: 32},
	}
	after := map[string]fileState{
		"src":           {mode: os.ModeDir | 0o755, isDir: true, modTime: time.Unix(2, 0), size: 64},
		"src/generated": {mode: 0o644, size: 1},
	}
	if got, want := changedFiles(before, after), []string{"src/generated"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("child creation reported the parent as a separate input change: got %v, want %v", got, want)
	}
	if got, want := changedFiles(after, before), []string{"src/generated"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("child deletion reported the parent as a separate input change: got %v, want %v", got, want)
	}
}

func TestDirectoryStructuralChangesRemainObservable(t *testing.T) {
	dir := fileState{mode: os.ModeDir | 0o755, isDir: true}
	for _, tc := range []struct {
		name          string
		before, after map[string]fileState
	}{
		{"create", nil, map[string]fileState{"src": dir}},
		{"delete", map[string]fileState{"src": dir}, nil},
		{"mode", map[string]fileState{"src": dir}, map[string]fileState{"src": {mode: os.ModeDir | 0o700, isDir: true}}},
		{"type", map[string]fileState{"src": dir}, map[string]fileState{"src": {mode: 0o644}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := changedFiles(tc.before, tc.after); !reflect.DeepEqual(got, []string{"src"}) {
				t.Fatalf("lost directory structural change: %v", got)
			}
		})
	}
}

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

func TestRunnerIgnoresNodeModulesByDefault(t *testing.T) {
	root := t.TempDir()
	moduleDir := filepath.Join(root, "node_modules", "pkg")
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(moduleDir, "index.js")
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
		t.Fatalf("unexpected node_modules batch: %+v", batch)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestRunnerCanWatchExplicitPathUnderDefaultIgnoredDir(t *testing.T) {
	root := t.TempDir()
	moduleDir := filepath.Join(root, "node_modules", "pkg")
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(moduleDir, "index.js")
	if err := os.WriteFile(path, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner, err := New(Options{
		Root:         root,
		Debounce:     40 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
		WatchPaths:   []string{"node_modules/pkg"},
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
		if !containsString(batch.Files, "node_modules/pkg/index.js") {
			t.Fatalf("unexpected explicit watch-path batch: %+v", batch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for explicit watch-path batch")
	}
}

func TestRunnerRestrictsPollingToWatchPaths(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(srcDir, "input.txt")
	otherPath := filepath.Join(root, "other.txt")
	if err := os.WriteFile(srcPath, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherPath, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner, err := New(Options{
		Root:         root,
		Debounce:     40 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
		WatchPaths:   []string{"src"},
	})
	if err != nil {
		t.Fatal(err)
	}
	batches, errs, err := runner.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(otherPath, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("watch error: %v", err)
		}
	case batch := <-batches:
		t.Fatalf("unexpected out-of-scope batch: %+v", batch)
	case <-time.After(250 * time.Millisecond):
	}

	if err := os.WriteFile(srcPath, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("watch error: %v", err)
		}
	case batch := <-batches:
		if !containsString(batch.Files, "src/input.txt") {
			t.Fatalf("unexpected watch-path batch: %+v", batch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scoped watch-path batch")
	}
}

func TestRunnerWatchOnlyCanObserveIncludeWithoutRoot(t *testing.T) {
	root := t.TempDir()
	includeDir := filepath.Join(root, ".devflow", "state", "instances", "abc", "flush", "sync")
	if err := os.MkdirAll(includeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(root, "other.txt")
	if err := os.WriteFile(otherPath, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner, err := New(Options{
		Root:         root,
		Debounce:     40 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
		WatchOnly:    true,
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
	if err := os.WriteFile(otherPath, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("watch error: %v", err)
		}
	case batch := <-batches:
		t.Fatalf("unexpected root batch while WatchOnly is set: %+v", batch)
	case <-time.After(250 * time.Millisecond):
	}

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
			t.Fatalf("unexpected include-only batch: %+v", batch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for include-only batch")
	}
}

func TestRunnerRepeatedWritesToPendingFileDoNotExtendDebounceForever(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sync", "flush.sync")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner, err := New(Options{
		Root:         root,
		Debounce:     120 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
		WatchPaths:   []string{"sync"},
	})
	if err != nil {
		t.Fatal(err)
	}
	batches, errs, err := runner.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(350 * time.Millisecond)
		i := 0
		for time.Now().Before(deadline) {
			i++
			_ = os.WriteFile(path, []byte(fmt.Sprintf("retouch-%d", i)), 0o644)
			time.Sleep(30 * time.Millisecond)
		}
	}()
	defer func() { <-done }()

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("watch error: %v", err)
		}
	case batch := <-batches:
		if !containsString(batch.Files, "sync/flush.sync") {
			t.Fatalf("unexpected repeated-write batch: %+v", batch)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("debounce was extended by repeated writes to the same pending file")
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

func TestRunnerWatchOnlyRetainsExplicitRoot(t *testing.T) {
	for _, absolute := range []bool{false, true} {
		t.Run(fmt.Sprintf("absolute=%t", absolute), func(t *testing.T) {
			root := t.TempDir()
			for _, rel := range []string{"main.go", "src/app.go", "node_modules/pkg/index.js", ".devflow/logs/task.log"} {
				path := filepath.Join(root, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("initial"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			watchRoot := "."
			if absolute {
				watchRoot = root
			}
			runner, err := New(Options{Root: root, WatchOnly: true, WatchPaths: []string{watchRoot}})
			if err != nil {
				t.Fatal(err)
			}
			before, err := runner.snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, rel := range []string{"main.go", "src/app.go"} {
				if _, ok := before[rel]; !ok {
					t.Errorf("root watch omitted %s", rel)
				}
			}
			for _, rel := range []string{"node_modules/pkg/index.js", ".devflow/logs/task.log"} {
				if _, ok := before[rel]; ok {
					t.Errorf("root watch included ignored path %s", rel)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("changed source"), 0o644); err != nil {
				t.Fatal(err)
			}
			after, err := runner.snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := changedFiles(before, after); !containsString(got, "main.go") {
				t.Fatalf("root source edit was not detected: %v", got)
			}
		})
	}
}

func TestRunnerRootWatchPreservesExplicitIgnoredSubtree(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "node_modules", "pkg", "index.js")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("module"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner, err := New(Options{Root: root, WatchOnly: true, WatchPaths: []string{".", "node_modules/pkg"}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runner.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshot["node_modules/pkg/index.js"]; !ok {
		t.Fatal("root watch swallowed the explicit ignored subtree")
	}
}

func TestRunnerCancellationUnblocksFullBatchQueue(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runner, err := New(Options{Root: root, PollInterval: time.Millisecond, Debounce: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		batches, errs, err := runner.Start(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Drain only during cleanup: cancellation must work without a consumer.
		defer func() {
			cancel()
			for range batches {
			}
		}()
		for i := 0; i <= cap(batches); i++ {
			if err := os.WriteFile(path, make([]byte, i+1), 0o644); err != nil {
				t.Fatal(err)
			}
			time.Sleep(3 * time.Millisecond)
			synctest.Wait()
		}
		if len(batches) != cap(batches) {
			t.Fatalf("batch queue did not fill: %d/%d", len(batches), cap(batches))
		}
		cancel()
		synctest.Wait()
		select {
		case err, ok := <-errs:
			if ok {
				t.Fatalf("unexpected watch error: %v", err)
			}
		default:
			t.Fatal("watcher remains blocked delivering a batch after cancellation")
		}
	})
}

func TestRunnerSyncIncludesChangesBeforeFirstPoll(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input.txt")
	if err := os.WriteFile(input, []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runner, err := New(Options{Root: root, PollInterval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		batches, _, err := runner.Start(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(input, []byte("changed before the first poll"), 0o644); err != nil {
			t.Fatal(err)
		}
		batch, err := runner.Sync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(batch.Files, []string{"input.txt"}) {
			t.Fatalf("fresh barrier missed input: %+v", batch)
		}
		if batch.StartedAt.IsZero() || batch.FinishedAt.Before(batch.StartedAt) {
			t.Fatalf("invalid batch timing: %+v", batch)
		}
		assertRunnerSynced(t, runner, batches, ctx)
	})
}

func TestRunnerSyncCombinesQueuedPendingAndUnpolledChanges(t *testing.T) {
	root := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runner, err := New(Options{Root: root, PollInterval: 10 * time.Millisecond, Debounce: 50 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		batches, _, err := runner.Start(ctx)
		if err != nil {
			t.Fatal(err)
		}
		writeWatchInput(t, root, "queued")
		time.Sleep(70 * time.Millisecond)
		synctest.Wait()
		if len(batches) != 1 {
			t.Fatalf("expected a queued batch, got %d", len(batches))
		}
		writeWatchInput(t, root, "pending")
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		writeWatchInput(t, root, "unpolled")
		batch, err := runner.Sync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"pending", "queued", "unpolled"}
		if !reflect.DeepEqual(batch.Files, want) {
			t.Fatalf("barrier lost outstanding changes: got %v, want %v", batch.Files, want)
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		assertRunnerSynced(t, runner, batches, ctx)
	})
}

func TestRunnerSyncDrainsFullQueueWithoutLosingChanges(t *testing.T) {
	root := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runner, err := New(Options{Root: root, PollInterval: time.Millisecond, Debounce: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		batches, _, err := runner.Start(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := make([]string, 0, cap(batches)+3)
		for i := 0; i < cap(batches)+3; i++ {
			name := fmt.Sprintf("input-%02d", i)
			want = append(want, name)
			writeWatchInput(t, root, name)
			time.Sleep(3 * time.Millisecond)
			synctest.Wait()
		}
		if len(batches) != cap(batches) {
			t.Fatalf("batch queue did not fill: %d/%d", len(batches), cap(batches))
		}
		canceled, cancelSync := context.WithCancel(ctx)
		cancelSync()
		if _, err := runner.Sync(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled barrier returned %v", err)
		}
		batch, err := runner.Sync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(batch.Files, want) {
			t.Fatalf("full-queue barrier lost changes: got %v, want %v", batch.Files, want)
		}
		time.Sleep(5 * time.Millisecond)
		synctest.Wait()
		assertRunnerSynced(t, runner, batches, ctx)
		cancel()
		synctest.Wait()
		if _, err := runner.Sync(context.Background()); !errors.Is(err, context.Canceled) {
			t.Fatalf("stopped watcher barrier returned %v", err)
		}
	})
}

func TestRunnerSyncScanFailureRetainsOutstandingChanges(t *testing.T) {
	root := t.TempDir()
	broken := filepath.Join(root, "broken")
	if err := os.Symlink("broken", broken); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	writeWatchInput(t, root, "broken")
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runner, err := New(Options{Root: root, WatchPaths: []string{".", "broken"}, PollInterval: 10 * time.Millisecond, Debounce: 50 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		batches, _, err := runner.Start(ctx)
		if err != nil {
			t.Fatal(err)
		}
		writeWatchInput(t, root, "queued")
		time.Sleep(70 * time.Millisecond)
		synctest.Wait()
		writeWatchInput(t, root, "pending")
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if len(batches) != 1 {
			t.Fatalf("expected a queued batch, got %d", len(batches))
		}
		if err := os.Remove(broken); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("broken", broken); err != nil {
			t.Fatal(err)
		}
		if batch, err := runner.Sync(ctx); err == nil || len(batch.Files) != 0 {
			t.Fatalf("failed snapshot reported successful work: batch=%+v, err=%v", batch, err)
		}
		if len(batches) != 1 {
			t.Fatal("failed snapshot consumed queued changes")
		}
		if err := os.Remove(broken); err != nil {
			t.Fatal(err)
		}
		writeWatchInput(t, root, "broken")
		batch, err := runner.Sync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"queued", "pending"} {
			if !containsString(batch.Files, want) {
				t.Fatalf("snapshot failure lost %s: %v", want, batch.Files)
			}
		}
		assertRunnerSynced(t, runner, batches, ctx)
	})
}

func writeWatchInput(t *testing.T, root, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertRunnerSynced(t *testing.T, runner *Runner, batches <-chan Batch, ctx context.Context) {
	t.Helper()
	batch, err := runner.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Files) != 0 {
		t.Fatalf("barrier returned already consumed work: %+v", batch)
	}
	select {
	case batch := <-batches:
		t.Fatalf("barrier left duplicate delivery: %+v", batch)
	default:
	}
}
