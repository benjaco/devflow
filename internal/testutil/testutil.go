package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TempWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".devflow"), 0o755); err != nil {
		t.Fatalf("mkdir .devflow: %v", err)
	}
	return dir
}
