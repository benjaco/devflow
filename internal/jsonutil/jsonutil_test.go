package jsonutil

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

type testPayload struct {
	Value int `json:"value"`
}

func TestWriteFileAtomicRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	if err := WriteFileAtomic(path, testPayload{Value: 42}); err != nil {
		t.Fatal(err)
	}
	var got testPayload
	if err := ReadFile(path, &got); err != nil {
		t.Fatal(err)
	}
	if got.Value != 42 {
		t.Fatalf("value = %d, want 42", got.Value)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm() & 0o077; got != 0 {
			t.Fatalf("state file exposes group/other permissions: %03o", got)
		}
	}
}

func TestWriteFileAtomicConcurrentWritersLeaveValidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	const writers = 32
	errCh := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(value int) {
			defer wg.Done()
			errCh <- WriteFileAtomic(path, testPayload{Value: value})
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent atomic write failed: %v", err)
		}
	}
	var got testPayload
	if err := ReadFile(path, &got); err != nil {
		t.Fatalf("final state is not valid JSON: %v", err)
	}
	if got.Value < 0 || got.Value >= writers {
		t.Fatalf("final value %d was not written by a test writer", got.Value)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".state.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files were not cleaned up: %v", matches)
	}
}

func TestWriteFileAtomicMarshalFailurePreservesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteFileAtomic(path, testPayload{Value: 7}); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, make(chan int)); err == nil {
		t.Fatal("expected unsupported JSON value to fail")
	}
	var got testPayload
	if err := ReadFile(path, &got); err != nil {
		t.Fatal(err)
	}
	if got.Value != 7 {
		t.Fatalf("failed replacement changed existing state: %+v", got)
	}
}
