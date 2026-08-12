package lock

import (
	"path/filepath"
	"testing"
	"time"
)

func TestFileLockSerializesAndReleaseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan *FileLock, 1)
	errs := make(chan error, 1)
	go func() {
		second, err := Acquire(path)
		if err != nil {
			errs <- err
			return
		}
		acquired <- second
	}()

	select {
	case second := <-acquired:
		_ = second.Release()
		t.Fatal("second lock acquired before first was released")
	case err := <-errs:
		t.Fatalf("second lock failed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case second := <-acquired:
		if err := second.Release(); err != nil {
			t.Fatal(err)
		}
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("second lock did not acquire after release")
	}
	if err := first.Release(); err != nil {
		t.Fatalf("repeated release should be harmless: %v", err)
	}
	var nilLock *FileLock
	if err := nilLock.Release(); err != nil {
		t.Fatalf("nil release should be harmless: %v", err)
	}
}
