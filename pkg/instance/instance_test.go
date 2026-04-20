package instance

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"devflow/pkg/api"
)

func TestResolveSameWorktreeSameInstance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()

	first, err := Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("instance ids differ: %s != %s", first.ID, second.ID)
	}
}

func TestSaveWritesRuntimeEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	inst := &api.Instance{
		ID:       "abc123",
		Label:    "test",
		Worktree: worktree,
		Env:      map[string]string{"FOO": "bar"},
		Ports:    map[string]int{},
	}
	if err := Save(inst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(worktree, ".devflow", "state", "instances", "abc123", "runtime.env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "FOO=bar\n" {
		t.Fatalf("runtime env contents = %q", string(data))
	}
}

func TestRecordDetachedRunPersistsSupervisorAndLastRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	inst, err := Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordDetachedRun(inst, api.RunConfig{
		Project:     "go-next-monorepo",
		Target:      "fullstack",
		Mode:        api.ModeWatch,
		MaxParallel: 2,
		Detached:    true,
	}, 4321, filepath.Join(worktree, "supervisor.log")); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Supervisor.PID != 4321 {
		t.Fatalf("unexpected supervisor pid: %d", loaded.Supervisor.PID)
	}
	if loaded.LastRun.Target != "fullstack" || !loaded.LastRun.Detached {
		t.Fatalf("unexpected last run: %+v", loaded.LastRun)
	}
}

func TestStopSupervisorIgnoresMissingProcess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	inst, err := Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	inst.Supervisor = api.SupervisorRef{PID: 999999}
	if err := Save(inst); err != nil {
		t.Fatal(err)
	}
	if err := StopSupervisor(inst); err != nil {
		t.Fatalf("expected missing supervisor process to be ignored, got %v", err)
	}
	loaded, err := Load(worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Supervisor.PID != 0 {
		t.Fatalf("expected supervisor to be cleared, got %+v", loaded.Supervisor)
	}
}

func TestCacheRootUsesGitCommonDirAcrossWorktrees(t *testing.T) {
	mainRepo, sibling := setupGitWorktrees(t)
	first := CacheRoot(mainRepo)
	second := CacheRoot(sibling)
	if first != second {
		t.Fatalf("expected shared cache root, got %q and %q", first, second)
	}
	commonDir, err := GitCommonDir(mainRepo)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(commonDir, "devflow", "cache")
	if first != want {
		t.Fatalf("unexpected cache root: got %q want %q", first, want)
	}
}

func TestCacheRootFallsBackOutsideGitRepo(t *testing.T) {
	worktree := t.TempDir()
	want := filepath.Join(worktree, ".devflow", "cache")
	if got := CacheRoot(worktree); got != want {
		t.Fatalf("unexpected cache root fallback: got %q want %q", got, want)
	}
}

func TestRepoSharedStateRootFallsBackOutsideGitRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()

	got, err := RepoSharedStateRoot(worktree)
	if err != nil {
		t.Fatal(err)
	}
	want, err := GlobalStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("unexpected shared state fallback: got %q want %q", got, want)
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
