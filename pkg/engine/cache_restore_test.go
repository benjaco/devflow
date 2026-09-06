package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

type cacheDeclarationProject struct {
	testProject
	task project.Task
}

func (p cacheDeclarationProject) Tasks() []project.Task { return []project.Task{p.task} }
func (p cacheDeclarationProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{p.task.Name}}}
}

func TestEngineRequiresCachedOutputsBeforeExecution(t *testing.T) {
	p := cacheDeclarationProject{task: project.Task{Name: "generate", Kind: project.KindOnce, Cache: true}}
	worktree := t.TempDir()
	if _, err := New(p, worktree); err == nil || !strings.Contains(err.Error(), "outputs") {
		t.Fatalf("cacheable task without outputs must be rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".devflow")); !os.IsNotExist(err) {
		t.Fatalf("invalid cached task caused worktree mutation: %v", err)
	}
	for _, outputs := range []project.Outputs{
		{Paths: []string{"dist"}},
		{Files: []string{"dist/app"}},
		{Dirs: []string{"dist"}},
	} {
		p.task.Outputs = outputs
		if _, err := New(p, worktree); err != nil {
			t.Fatalf("valid output declaration rejected: %v", err)
		}
	}
	p.task = project.Task{Name: "install", Kind: project.KindOnce, Stamp: true}
	if _, err := New(p, worktree); err != nil {
		t.Fatalf("local install stamps may omit outputs: %v", err)
	}
}

func TestRunDoesNotExecuteAfterCacheRestoreOperationalFailure(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	runs := 0
	p := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("cache-restore-failure")
		build := b.Task("build").Run(func(_ context.Context, rt *project.Runtime) error {
			runs++
			if err := os.MkdirAll(rt.Abs("dist"), 0o755); err != nil {
				return err
			}
			return os.WriteFile(rt.Abs("dist/result.txt"), []byte("built artifact"), 0o600)
		}).OutputFiles("dist/result.txt")
		b.Target("build", build)
		return nil
	})
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Target: "build", Worktree: worktree, Mode: api.ModeCI}
	if _, err := eng.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(worktree, "dist")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "dist"), []byte("preserve this file"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := eng.Run(context.Background(), req)
	if err == nil || out == nil || out.Result.Success || out.Result.FailedNode != "build" {
		t.Fatalf("operational restore error was not a failed task: outcome=%+v error=%v", out, err)
	}
	if runs != 1 {
		t.Fatalf("task executed after an unsafe cache restore: runs=%d", runs)
	}
	contents, err := os.ReadFile(filepath.Join(worktree, "dist"))
	if err != nil || string(contents) != "preserve this file" {
		t.Fatalf("restore changed the obstructing user file: contents=%q error=%v", contents, err)
	}
}
