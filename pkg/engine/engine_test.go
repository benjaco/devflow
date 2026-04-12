package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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

type parallelProject struct {
	maxSeen atomic.Int32
	current atomic.Int32
}

func (p *parallelProject) Name() string { return "parallel-project" }

func (p *parallelProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "parallel"}, nil
}

func (p *parallelProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"join"}}}
}

func (p *parallelProject) Tasks() []project.Task {
	makeTask := func(name string) project.Task {
		return project.Task{
			Name: name,
			Kind: project.KindOnce,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = rt
				current := p.current.Add(1)
				defer p.current.Add(-1)
				for {
					max := p.maxSeen.Load()
					if current <= max || p.maxSeen.CompareAndSwap(max, current) {
						break
					}
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(120 * time.Millisecond):
					return nil
				}
			},
		}
	}

	return []project.Task{
		makeTask("a"),
		makeTask("b"),
		{
			Name: "join",
			Kind: project.KindOnce,
			Deps: []string{"a", "b"},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				_ = rt
				return nil
			},
		},
	}
}

func TestRunParallelizesIndependentTasks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	p := &parallelProject{}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI, MaxParallel: 2}); err != nil {
		t.Fatal(err)
	}
	if got := p.maxSeen.Load(); got < 2 {
		t.Fatalf("expected parallel execution, max concurrent = %d", got)
	}
}

func TestRunHonorsMaxParallelOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	p := &parallelProject{}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI, MaxParallel: 1}); err != nil {
		t.Fatal(err)
	}
	if got := p.maxSeen.Load(); got != 1 {
		t.Fatalf("expected max concurrency 1, got %d", got)
	}
}
