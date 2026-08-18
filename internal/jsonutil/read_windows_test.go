//go:build windows

package jsonutil

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestReadFileRetriesWindowsSharingViolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteFileAtomic(path, testPayload{Value: 42}); err != nil {
		t.Fatal(err)
	}

	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = windows.CloseHandle(handle)
		close(closed)
	}()

	var got testPayload
	readErr := ReadFile(path, &got)
	<-closed
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got.Value != 42 {
		t.Fatalf("value = %d, want 42", got.Value)
	}
}
