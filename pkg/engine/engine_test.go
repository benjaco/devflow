package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"devflow/pkg/api"
	"devflow/pkg/project"
)

type testProject struct{}

func (testProject) Name() string { return "test-project" }

func (testProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "test"}, nil
}

func (testProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:      "gen",
			Kind:      project.KindOnce,
			Cache:     true,
			Inputs:    project.Inputs{Files: []string{"input.txt"}},
			Outputs:   project.Outputs{Files: []string{"out.txt"}},
			Signature: "gen-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				data, err := os.ReadFile(filepath.Join(rt.Worktree, "input.txt"))
				if err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(rt.Worktree, "out.txt"), data, 0o644)
			},
		},
	}
}

func (testProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"gen"}}}
}

func TestRunUsesCacheOnSecondExecution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, err := New(testProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	first, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Result.CacheHits) != 0 {
		t.Fatalf("unexpected cache hits on first run: %v", first.Result.CacheHits)
	}
	second, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Result.CacheHits) != 1 || second.Result.CacheHits[0] != "gen" {
		t.Fatalf("unexpected cache hits on second run: %v", second.Result.CacheHits)
	}
}
