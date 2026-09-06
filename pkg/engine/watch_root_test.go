package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

type rootGlobWatchProject struct {
	pattern  string
	input    string
	observed chan string
}

func (p rootGlobWatchProject) Name() string { return "root-glob-watch" }

func (p rootGlobWatchProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{}, nil
}

func (p rootGlobWatchProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"observe"}}}
}

func (p rootGlobWatchProject) Tasks() []project.Task {
	return []project.Task{{
		Name:   "observe",
		Kind:   project.KindOnce,
		Inputs: project.Inputs{Globs: []string{p.pattern}},
		Run: func(ctx context.Context, rt *project.Runtime) error {
			contents, err := os.ReadFile(filepath.Join(rt.Worktree, filepath.FromSlash(p.input)))
			if err != nil {
				return err
			}
			select {
			case p.observed <- string(contents):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}}
}

func TestWatchRerunsRootGlobInputs(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("HOME", cacheRoot)
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("LOCALAPPDATA", cacheRoot)
	for _, test := range []struct {
		pattern string
		input   string
	}{
		{pattern: "*.txt", input: "input.txt"},
		{pattern: "**/*.txt", input: "src/input.txt"},
	} {
		t.Run(test.pattern, func(t *testing.T) {
			worktree := t.TempDir()
			inputPath := filepath.Join(worktree, filepath.FromSlash(test.input))
			if err := os.MkdirAll(filepath.Dir(inputPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(inputPath, []byte("initial"), 0o644); err != nil {
				t.Fatal(err)
			}
			p := rootGlobWatchProject{pattern: test.pattern, input: test.input, observed: make(chan string, 8)}
			eng, err := New(p, worktree)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- eng.Watch(ctx, Request{Target: "dev", Worktree: worktree, Mode: api.ModeWatch})
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("watch returned an error: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Error("watch did not stop after cancellation")
				}
			})
			waitForEngineWatchReady(t, worktree)
			select {
			case got := <-p.observed:
				if got != "initial" {
					t.Fatalf("initial run observed %q", got)
				}
			default:
				t.Fatal("initial watch run did not execute the task")
			}
			if err := os.WriteFile(inputPath, []byte("changed source"), 0o644); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-p.observed:
				if got != "changed source" {
					t.Fatalf("watch rerun observed %q", got)
				}
			case <-time.After(4 * time.Second):
				t.Fatalf("watch did not rerun after editing %s declared by root glob %s", test.input, test.pattern)
			}
		})
	}
}
