package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func TestLinkedWorktreeDotEnvFirstRunUsesMainFile(t *testing.T) {
	isolateEngineUserCache(t)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent-gitconfig"))
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "DEVFLOW_TEST_WORKTREE_SOURCE"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	base := t.TempDir()
	mainRoot, linkedRoot := filepath.Join(base, "main"), filepath.Join(base, "linked")
	runDotEnvGitCommand(t, base, "init", mainRoot)
	runDotEnvGitCommand(t, mainRoot, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	runDotEnvGitCommand(t, mainRoot, "worktree", "add", "--detach", linkedRoot, "HEAD")

	// Writing .env after worktree add reproduces the untracked-file setup.
	mainEnv := "DEVFLOW_TEST_WORKTREE_SOURCE=main\n"
	writeEngineDotEnvFile(t, filepath.Join(mainRoot, ".env"), mainEnv)
	var taskEnv map[string]string
	var taskWorktree, taskInstanceID string
	p := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("worktree-env").DotEnv(".env")
		task := b.Task("check").NoCache().Run(func(_ context.Context, rt *project.Runtime) error {
			taskEnv = cloneMap(rt.Env)
			taskWorktree, taskInstanceID = rt.Worktree, rt.Instance.ID
			return nil
		})
		b.Target("verify", task)
		return nil
	})
	eng, err := New(p, linkedRoot)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := eng.Run(context.Background(), Request{Worktree: linkedRoot, Target: "verify", Mode: api.ModeCI})
	if err != nil || !outcome.Result.Success {
		t.Fatalf("first linked-worktree run failed: result=%+v error=%v", outcome.Result, err)
	}

	if got := taskEnv["DEVFLOW_TEST_WORKTREE_SOURCE"]; got != "main" {
		t.Errorf("first task received SOURCE=%q; missing local .env should use main", got)
	}
	wantID, canonicalRoot, err := instance.IDForWorktree(linkedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if taskWorktree != linkedRoot || taskInstanceID != wantID {
		t.Errorf("task ran in %q with instance %q; want linked worktree %q with instance %q", taskWorktree, taskInstanceID, linkedRoot, wantID)
	}
	if outcome.Instance.Worktree != canonicalRoot || outcome.Result.InstanceID != wantID {
		t.Errorf("run recorded ownership outside the linked worktree: %+v", outcome.Instance)
	}
	persisted, err := instance.Load(linkedRoot, wantID)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Env["DEVFLOW_TEST_WORKTREE_SOURCE"]; got != "main" {
		t.Errorf("linked instance persisted SOURCE=%q; want main", got)
	}
	runtimeFile, err := project.LoadDotEnv(filepath.Join(linkedRoot, ".devflow", "state", "instances", wantID, "runtime.env"))
	if err != nil {
		t.Fatal(err)
	}
	if got := runtimeFile["DEVFLOW_TEST_WORKTREE_SOURCE"]; got != "main" {
		t.Errorf("linked runtime.env persisted SOURCE=%q; want main", got)
	}
	if _, err := os.Stat(filepath.Join(linkedRoot, ".env")); !os.IsNotExist(err) {
		t.Errorf("fallback must not copy .env into linked worktree; stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mainRoot, ".devflow")); !os.IsNotExist(err) {
		t.Errorf("running in linked worktree must not create main-checkout state; stat error: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(mainRoot, ".env")); err != nil || string(got) != mainEnv {
		t.Errorf("main .env was modified: content=%q error=%v", got, err)
	}
}

func TestLinkedWorktreeDotEnvLocalFileOverridesPreviousFallback(t *testing.T) {
	isolateEngineUserCache(t)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent-gitconfig"))
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "DEVFLOW_TEST_WORKTREE_SOURCE"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	base := t.TempDir()
	mainRoot, linkedRoot := filepath.Join(base, "main"), filepath.Join(base, "linked")
	runDotEnvGitCommand(t, base, "init", mainRoot)
	runDotEnvGitCommand(t, mainRoot, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	runDotEnvGitCommand(t, mainRoot, "worktree", "add", "--detach", linkedRoot, "HEAD")
	mainEnv := "DEVFLOW_TEST_WORKTREE_SOURCE=main\n"
	writeEngineDotEnvFile(t, filepath.Join(mainRoot, ".env"), mainEnv)

	var taskSource string
	p := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("worktree-env").DotEnv(".env")
		task := b.Task("check").NoCache().Run(func(_ context.Context, rt *project.Runtime) error {
			taskSource = rt.Env["DEVFLOW_TEST_WORKTREE_SOURCE"]
			return nil
		})
		b.Target("verify", task)
		return nil
	})
	eng, err := New(p, linkedRoot)
	if err != nil {
		t.Fatal(err)
	}
	first, err := eng.Run(context.Background(), Request{Worktree: linkedRoot, Target: "verify", Mode: api.ModeCI})
	if err != nil || !first.Result.Success || taskSource != "main" {
		t.Fatalf("setup run must use main fallback before adding local .env: SOURCE=%q result=%+v error=%v", taskSource, first.Result, err)
	}
	localEnv := "DEVFLOW_TEST_WORKTREE_SOURCE=linked\n"
	writeEngineDotEnvFile(t, filepath.Join(linkedRoot, ".env"), localEnv)

	second, err := eng.Run(context.Background(), Request{Worktree: linkedRoot, Target: "verify", Mode: api.ModeCI})
	if err != nil || !second.Result.Success {
		t.Fatalf("run after adding local .env failed: result=%+v error=%v", second.Result, err)
	}

	if taskSource != "linked" {
		t.Errorf("next task received SOURCE=%q; local .env should override previous main fallback", taskSource)
	}
	if second.Result.InstanceID != first.Result.InstanceID {
		t.Errorf("adding local .env changed instance identity: %q -> %q", first.Result.InstanceID, second.Result.InstanceID)
	}
	persisted, err := instance.Load(linkedRoot, first.Result.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Env["DEVFLOW_TEST_WORKTREE_SOURCE"]; got != "linked" {
		t.Errorf("linked instance persisted SOURCE=%q; want linked", got)
	}
	runtimeFile, err := project.LoadDotEnv(filepath.Join(linkedRoot, ".devflow", "state", "instances", first.Result.InstanceID, "runtime.env"))
	if err != nil {
		t.Fatal(err)
	}
	if got := runtimeFile["DEVFLOW_TEST_WORKTREE_SOURCE"]; got != "linked" {
		t.Errorf("linked runtime.env persisted SOURCE=%q; want linked", got)
	}
	if got, err := os.ReadFile(filepath.Join(linkedRoot, ".env")); err != nil || string(got) != localEnv {
		t.Errorf("local .env was modified: content=%q error=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(mainRoot, ".env")); err != nil || string(got) != mainEnv {
		t.Errorf("main .env was modified: content=%q error=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(mainRoot, ".devflow")); !os.IsNotExist(err) {
		t.Errorf("local override must not create main-checkout state; stat error: %v", err)
	}
}

func TestLinkedWorktreeDotEnvPreservesEnvironmentPrecedence(t *testing.T) {
	isolateEngineUserCache(t)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent-gitconfig"))
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "DEVFLOW_TEST_WORKTREE_SOURCE", "DEVFLOW_TEST_WORKTREE_STATIC"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	base := t.TempDir()
	mainRoot, linkedRoot := filepath.Join(base, "main"), filepath.Join(base, "linked")
	runDotEnvGitCommand(t, base, "init", mainRoot)
	runDotEnvGitCommand(t, mainRoot, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	runDotEnvGitCommand(t, mainRoot, "worktree", "add", "--detach", linkedRoot, "HEAD")
	writeEngineDotEnvFile(t, filepath.Join(mainRoot, ".env"), `DEVFLOW_TEST_WORKTREE_SOURCE=main
DEVFLOW_TEST_WORKTREE_PROCESS=main
DEVFLOW_TEST_WORKTREE_STATIC=main
DEVFLOW_TEST_WORKTREE_MANAGED=main
DEVFLOW_INSTANCE_ID=main
DEVFLOW_WORKTREE=main
`)
	prior, err := instance.Resolve(linkedRoot, "linked")
	if err != nil {
		t.Fatal(err)
	}
	prior.Env["DEVFLOW_TEST_WORKTREE_SOURCE"] = "persisted"
	if err := instance.Save(prior); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVFLOW_TEST_WORKTREE_PROCESS", "process")
	t.Setenv("DEVFLOW_TEST_WORKTREE_MANAGED", "process")
	t.Setenv("DEVFLOW_INSTANCE_ID", "process")
	t.Setenv("DEVFLOW_WORKTREE", "process")

	var taskEnv map[string]string
	p := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("worktree-env").DotEnv(".env")
		b.Env("DEVFLOW_TEST_WORKTREE_STATIC", "adapter")
		b.Finalize(func(inst *api.Instance) error {
			inst.Env["DEVFLOW_TEST_WORKTREE_MANAGED"] = "finalized"
			return nil
		})
		task := b.Task("check").NoCache().InputEnv("DEVFLOW_TEST_WORKTREE_PROCESS").Run(func(_ context.Context, rt *project.Runtime) error {
			taskEnv = cloneMap(rt.Env)
			return nil
		})
		b.Target("verify", task)
		return nil
	})
	eng, err := New(p, linkedRoot)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := eng.Run(context.Background(), Request{Worktree: linkedRoot, Target: "verify", Mode: api.ModeCI})
	if err != nil || !outcome.Result.Success {
		t.Fatalf("precedence run failed: result=%+v error=%v", outcome.Result, err)
	}

	want := map[string]string{
		"DEVFLOW_TEST_WORKTREE_SOURCE":  "main",      // Fresh defaults replace persisted recovery values.
		"DEVFLOW_TEST_WORKTREE_PROCESS": "process",   // Declared process input overrides dotenv.
		"DEVFLOW_TEST_WORKTREE_STATIC":  "adapter",   // Explicit adapter defaults override dotenv.
		"DEVFLOW_TEST_WORKTREE_MANAGED": "finalized", // Managed values override both dotenv and process.
		"DEVFLOW_INSTANCE_ID":           prior.ID,
		"DEVFLOW_WORKTREE":              prior.Worktree,
	}
	persisted, err := instance.Load(linkedRoot, prior.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtimeFile, err := project.LoadDotEnv(filepath.Join(linkedRoot, ".devflow", "state", "instances", prior.ID, "runtime.env"))
	if err != nil {
		t.Fatal(err)
	}
	for key, expected := range want {
		if got := taskEnv[key]; got != expected {
			t.Errorf("task env %s=%q; want %q", key, got, expected)
		}
		if got := persisted.Env[key]; got != expected {
			t.Errorf("linked instance persisted %s=%q; want %q", key, got, expected)
		}
		if got := runtimeFile[key]; got != expected {
			t.Errorf("linked runtime.env persisted %s=%q; want %q", key, got, expected)
		}
	}
}

func runDotEnvGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func writeEngineDotEnvFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
