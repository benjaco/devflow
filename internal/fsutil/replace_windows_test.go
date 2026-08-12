//go:build windows

package fsutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestReplaceFileRetriesWindowsSharingViolation(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "state.json")
	temporary := filepath.Join(dir, ".state.json.tmp-retry")
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	destinationPtr, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		destinationPtr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
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

	replaceErr := ReplaceFile(temporary, destination)
	<-closed
	if replaceErr != nil {
		t.Fatal(replaceErr)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("replacement contents = %q, want new", data)
	}
}
