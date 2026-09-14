package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTUIBootstrapLogRoutingDoesNotCreateState(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{nil, {"tui", "--target", "up"}} {
		if path := tuiBootstrapLogPath(root, args); filepath.Base(path) != "tui.log" {
			t.Fatalf("TUI bootstrap lost diagnostic destination: %v: %q", args, path)
		}
	}
	for _, args := range [][]string{{"run", "verify", "--ci", "--json"}, {"logs", "tui", "--json"}, {"status", "--json"}, {"__internal_daemon"}} {
		if path := tuiBootstrapLogPath(root, args); path != "" {
			t.Fatalf("non-TUI invocation selected diagnostics: %v: %q", args, path)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".devflow")); !os.IsNotExist(err) {
		t.Fatalf("routing created runtime state: %v", err)
	}
}

func TestBootstrapLogAppendsAndReportsStorageErrors(t *testing.T) {
	root := t.TempDir()
	path := tuiBootstrapLogPath(root, nil)
	var stderr bytes.Buffer
	writeBootstrapLog(path, &stderr, "bootstrap_child_started", "child_pid=42")
	writeBootstrapLog(path, &stderr, "bootstrap_child_exit_observed", "child_pid=42 native_exit=7")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 || strings.Count(string(data), "\n") != 2 || !strings.Contains(string(data), fmt.Sprintf("pid=%d ppid=%d", os.Getpid(), os.Getppid())) {
		t.Fatalf("parent evidence missing: stderr=%s log=%s", &stderr, data)
	}
	writeBootstrapLog("", &stderr, "disabled", "")
	if stderr.Len() != 0 {
		t.Fatalf("disabled diagnostics emitted output: %s", &stderr)
	}
	// A regular file cannot be the parent directory, on every supported OS.
	writeBootstrapLog(filepath.Join(path, "blocked.log"), &stderr, "unwritable", "")
	if !strings.Contains(stderr.String(), "devflow bootstrap diagnostics:") {
		t.Fatalf("storage failure was hidden: %s", &stderr)
	}
}
