package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func BuildTestCommand(t *testing.T) string {
	t.Helper()
	root := RepoRoot(t)
	bin := filepath.Join(t.TempDir(), "testcmd"+ExeSuffix())
	cmd := exec.Command("go", "build", "-o", bin, "./internal/testutil/testcmd")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build test command: %v\n%s", err, string(out))
	}
	return bin
}

func RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve repo root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func ExeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
