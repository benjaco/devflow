package ports

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"devflow/pkg/instance"
)

func TestAllocateUniquePorts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	manager, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Allocate("one", []string{"backend", "frontend"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Allocate("two", []string{"backend", "frontend"})
	if err != nil {
		t.Fatal(err)
	}
	if first["backend"] == second["backend"] || first["frontend"] == second["frontend"] {
		t.Fatalf("ports collided: first=%v second=%v", first, second)
	}
}

func TestNewDefaultForWorktreeUsesGitCommonDirAcrossWorktrees(t *testing.T) {
	mainRepo, sibling := setupGitWorktrees(t)
	first, err := NewDefaultForWorktree(mainRepo)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewDefaultForWorktree(sibling)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path != second.Path {
		t.Fatalf("expected shared port registry path, got %q and %q", first.Path, second.Path)
	}
	commonDir, err := instance.GitCommonDir(mainRepo)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(commonDir, "devflow", "state", "ports.json")
	if first.Path != want {
		t.Fatalf("unexpected port registry path: got %q want %q", first.Path, want)
	}
}

func TestNewDefaultForWorktreeFallsBackOutsideGitRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()

	manager, err := NewDefaultForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	root, err := instance.GlobalStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "ports.json")
	if manager.Path != want {
		t.Fatalf("unexpected fallback port registry path: got %q want %q", manager.Path, want)
	}
}

func setupGitWorktrees(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	mainRepo := filepath.Join(base, "repo")
	sibling := filepath.Join(base, "repo-wt")
	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, mainRepo, "init")
	runGit(t, mainRepo, "config", "user.email", "devflow@example.com")
	runGit(t, mainRepo, "config", "user.name", "Devflow Test")
	if err := os.WriteFile(filepath.Join(mainRepo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, mainRepo, "add", "README.md")
	runGit(t, mainRepo, "commit", "-m", "init")
	runGit(t, mainRepo, "worktree", "add", "-b", "feature", sibling, "HEAD")
	return mainRepo, sibling
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}
