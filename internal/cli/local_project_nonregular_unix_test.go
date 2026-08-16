//go:build linux || darwin

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLocalProjectSourceFilesRejectsMatchingSpecialFile(t *testing.T) {
	worktree := t.TempDir()
	projectPath := filepath.Join(worktree, localProjectFile)
	if err := os.WriteFile(projectPath, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	matchingPath := filepath.Join(worktree, "devflow_pipe.go")
	if err := unix.Mkfifo(matchingPath, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := localProjectSourceFiles(projectPath)
	if err == nil || !strings.Contains(err.Error(), matchingPath) || !strings.Contains(err.Error(), "expected a regular Go source file") {
		t.Fatalf("expected path-specific special-file error, got %v", err)
	}
}
