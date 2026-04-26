package instance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
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

func TestFlushRequestAndAckRoundTrip(t *testing.T) {
	worktree := t.TempDir()
	instanceID := "abc123"
	requestID := "flush-1"
	syncPath := FlushSyncPath(worktree, instanceID, requestID)
	req := api.FlushRequest{
		ID:        requestID,
		CreatedAt: time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		SyncPath:  syncPath,
	}
	if err := WriteFlushRequest(worktree, instanceID, req); err != nil {
		t.Fatal(err)
	}
	loadedReq, err := LoadFlushRequest(worktree, instanceID, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedReq.ID != req.ID || loadedReq.SyncPath != req.SyncPath || !loadedReq.CreatedAt.Equal(req.CreatedAt) {
		t.Fatalf("unexpected loaded request: %+v", loadedReq)
	}

	result := api.FlushResult{
		RequestID:  requestID,
		InstanceID: instanceID,
		Worktree:   worktree,
		Target:     "dev",
		Mode:       api.ModeWatch,
		Synced:     true,
		Success:    true,
		UpdatedAt:  req.CreatedAt,
	}
	if err := WriteFlushAck(worktree, instanceID, result); err != nil {
		t.Fatal(err)
	}
	loadedAck, err := LoadFlushAck(worktree, instanceID, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedAck.RequestID != result.RequestID || !loadedAck.Success || !loadedAck.Synced {
		t.Fatalf("unexpected loaded ack: %+v", loadedAck)
	}
	if got, want := FlushRequestPath(worktree, instanceID, requestID), filepath.Join(worktree, ".devflow", "state", "instances", instanceID, "flush", "requests", requestID+".json"); got != want {
		t.Fatalf("unexpected request path: got %q want %q", got, want)
	}
	if got, want := FlushAckPath(worktree, instanceID, requestID), filepath.Join(worktree, ".devflow", "state", "instances", instanceID, "flush", "acks", requestID+".json"); got != want {
		t.Fatalf("unexpected ack path: got %q want %q", got, want)
	}
	if got, want := FlushSyncPath(worktree, instanceID, requestID), filepath.Join(worktree, ".devflow", "state", "instances", instanceID, "flush", "sync", requestID+".sync"); got != want {
		t.Fatalf("unexpected sync path: got %q want %q", got, want)
	}
	if got, want := FlushWatchReadyPath(worktree, instanceID), filepath.Join(worktree, ".devflow", "state", "instances", instanceID, "flush", "watch.ready"); got != want {
		t.Fatalf("unexpected watch ready path: got %q want %q", got, want)
	}
}

func TestCacheRootUsesSingleUserCacheAcrossWorktrees(t *testing.T) {
	mainRepo, sibling := setupGitWorktrees(t)
	first := CacheRoot()
	second := CacheRoot()
	if first != second {
		t.Fatalf("expected single cache root, got %q and %q", first, second)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "devflow", "cache")
	if first != want {
		t.Fatalf("unexpected cache root: got %q want %q", first, want)
	}
	for _, worktree := range []string{mainRepo, sibling} {
		if strings.Contains(first, worktree) {
			t.Fatalf("cache root %q should not live under worktree %q", first, worktree)
		}
	}
}

func TestCacheRootDoesNotUseLocalDevflowCacheOutsideGitRepo(t *testing.T) {
	worktree := t.TempDir()
	got := CacheRoot()
	if got == filepath.Join(worktree, ".devflow", "cache") {
		t.Fatalf("cache root should be global, got local worktree cache %q", got)
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
