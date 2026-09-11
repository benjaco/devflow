package fsutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestRenameRetryBackoffThenSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var attempts []time.Duration
		conflict := &os.LinkError{Op: "rename", Old: "source", New: "destination", Err: syscall.Errno(32)}
		err := renameWithRetry(context.Background(), "source", "destination", func(source, destination string) error {
			if source != "source" || destination != "destination" {
				t.Fatalf("rename paths changed: %q -> %q", source, destination)
			}
			attempts = append(attempts, time.Since(start))
			if len(attempts) < 4 {
				return conflict
			}
			return nil
		}, func(err error) bool { return errors.Is(err, syscall.Errno(32)) })
		if err != nil {
			t.Fatal(err)
		}
		if want := []time.Duration{0, time.Millisecond, 3 * time.Millisecond, 7 * time.Millisecond}; !slices.Equal(attempts, want) {
			t.Fatalf("rename attempt times = %v, want %v", attempts, want)
		}
	})
}

func TestRenamePersistentConflictIsBoundedAndDiagnosable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var attempts []time.Duration
		conflict := &os.LinkError{Op: "rename", Old: "source", New: "destination", Err: syscall.Errno(32)}
		err := renameWithRetry(context.Background(), "source", "destination", func(string, string) error {
			attempts = append(attempts, time.Since(start))
			return conflict
		}, func(err error) bool { return errors.Is(err, syscall.Errno(32)) })
		if time.Since(start) != 2*time.Second {
			t.Fatalf("retry budget = %v, want 2s", time.Since(start))
		}
		for i := 1; i < len(attempts); i++ {
			if delay := attempts[i] - attempts[i-1]; delay <= 0 || delay > 50*time.Millisecond {
				t.Fatalf("retry delay %v is outside policy", delay)
			}
		}
		if !errors.Is(err, syscall.Errno(32)) || !strings.Contains(err.Error(), "native error 32") {
			t.Fatalf("native cause/code hidden: %v", err)
		}
		for _, want := range []string{`rename "source" to "destination"`, fmt.Sprintf("after %d retries (2s", len(attempts)-1), "path may be locked or inaccessible"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("rename diagnostic omitted %q: %v", want, err)
			}
		}
		var native *os.LinkError
		if !errors.As(err, &native) || native != conflict {
			t.Fatalf("native rename operation hidden: %v", err)
		}
	})
}

func TestRenamePermanentFailureDoesNotRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0
		err := renameWithRetry(context.Background(), "source", "destination", func(string, string) error {
			calls++
			return os.ErrNotExist
		}, func(err error) bool { return errors.Is(err, syscall.Errno(32)) })
		if calls != 1 || time.Since(start) != 0 || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "after 0 retries (0s)") {
			t.Fatalf("permanent error retried or hidden: calls=%d err=%v", calls, err)
		}
	})
}

func TestRenameCancellationPreservesLastNativeFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		finished := make(chan struct{})
		go func() { time.Sleep(12 * time.Millisecond); cancel(); close(finished) }()
		defer func() { <-finished }()
		calls := 0
		conflict := &os.LinkError{Op: "rename", Old: "source", New: "destination", Err: syscall.Errno(32)}
		err := renameWithRetry(ctx, "source", "destination", func(string, string) error {
			calls++
			return conflict
		}, func(err error) bool { return errors.Is(err, syscall.Errno(32)) })
		if calls != 4 || time.Since(start) != 12*time.Millisecond || err == nil || !strings.Contains(err.Error(), "after 3 retries (12ms;") {
			t.Fatalf("cancellation did not interrupt retry: calls=%d err=%v", calls, err)
		}
		var native *os.LinkError
		if !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.Errno(32)) || !errors.As(err, &native) || native != conflict {
			t.Fatalf("cancellation/native causes missing: %v", err)
		}
	})
}

func TestRenameAlreadyCanceledDoesNotCallFilesystem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := renameWithRetry(ctx, "source", "destination", func(string, string) error {
		calls++
		return nil
	}, func(error) bool { return true })
	if calls != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled rename touched filesystem: calls=%d err=%v", calls, err)
	}
}

func TestRenameDeadlineInterruptsRetryDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		// Retry wakes occur at 1ms and 3ms; expire strictly inside that wait so
		// this checks interruption rather than scheduling of simultaneous timers.
		const deadline = 2500 * time.Microsecond
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		calls := 0
		err := renameWithRetry(ctx, "source", "destination", func(string, string) error {
			calls++
			return syscall.Errno(32)
		}, func(err error) bool { return errors.Is(err, syscall.Errno(32)) })
		if calls != 2 || time.Since(start) != deadline || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, syscall.Errno(32)) {
			t.Fatalf("deadline did not interrupt retry: calls=%d err=%v", calls, err)
		}
	})
}

func TestMoveRetryDoesNotRepeatDestinationRemoval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		t.Cleanup(func() { _ = RemoveAllWritable(root) })
		source := filepath.Join(root, "source")
		destination := filepath.Join(root, "holding", "output")
		if err := os.Mkdir(source, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "content"), []byte("original"), 0o644); err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" {
			if err := os.Chmod(source, 0o555); err != nil {
				t.Fatal(err)
			}
		}
		calls := 0
		var contender os.FileInfo
		err := movePathWritable(context.Background(), source, destination, func(string, string) error {
			calls++
			if calls == 1 {
				if err := os.WriteFile(destination, []byte("preserve contender"), 0o644); err != nil {
					t.Fatal(err)
				}
				var err error
				contender, err = os.Stat(destination)
				if err != nil {
					t.Fatal(err)
				}
				return syscall.Errno(32)
			}
			info, err := os.Stat(destination)
			if err != nil || !os.SameFile(contender, info) {
				t.Fatalf("retry removed or replaced a newly appearing destination: %v", err)
			}
			return os.ErrExist
		}, func(err error) bool { return errors.Is(err, syscall.Errno(32)) })
		if calls != 2 || !errors.Is(err, os.ErrExist) {
			t.Fatalf("unexpected retry result: calls=%d err=%v", calls, err)
		}
		if data, err := os.ReadFile(destination); err != nil || string(data) != "preserve contender" {
			t.Fatalf("destination changed after failed move: %q %v", data, err)
		}
		if data, err := os.ReadFile(filepath.Join(source, "content")); err != nil || string(data) != "original" {
			t.Fatalf("source changed after failed move: %q %v", data, err)
		}
		if runtime.GOOS != "windows" {
			info, err := os.Stat(source)
			if err != nil || info.Mode().Perm() != 0o555 {
				t.Fatalf("source mode was not restored after retry failure: %v %v", info, err)
			}
		}
	})
}

func TestCanceledMovePreservesSourceAndDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("destination"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := MovePathWritable(ctx, source, destination); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled move returned %v", err)
	}
	for path, want := range map[string]string{source: "source", destination: "destination"} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Fatalf("canceled move changed %q: %q %v", path, data, err)
		}
	}
}
