package project

import (
	"context"
	"strings"
	"sync"
	"testing"
)

type resolveExecutionProject struct{}

func (resolveExecutionProject) Name() string { return "resolve-execution" }
func (resolveExecutionProject) Tasks() []Task {
	return []Task{
		{Name: "build", Kind: KindOnce},
		{Name: "serve", Kind: KindService, Deps: []string{"build"}},
	}
}
func (resolveExecutionProject) Targets() []Target {
	return []Target{{Name: "fullstack", RootTasks: []string{"serve"}}}
}
func (resolveExecutionProject) ConfigureInstance(context.Context, string) (InstanceConfig, error) {
	return InstanceConfig{}, nil
}

func TestResolveExecutionProjectKeepsDeclaredTarget(t *testing.T) {
	base := resolveExecutionProject{}
	gotProject, gotTarget, err := ResolveExecutionProject(base, "fullstack")
	if err != nil {
		t.Fatal(err)
	}
	if gotTarget != "fullstack" {
		t.Fatalf("unexpected target %q", gotTarget)
	}
	if len(gotProject.Targets()) != 1 {
		t.Fatalf("expected original targets only, got %+v", gotProject.Targets())
	}
}

func TestResolveExecutionProjectWrapsTaskAsSyntheticTarget(t *testing.T) {
	base := resolveExecutionProject{}
	gotProject, gotTarget, err := ResolveExecutionProject(base, "build")
	if err != nil {
		t.Fatal(err)
	}
	if gotTarget != "build" {
		t.Fatalf("unexpected target %q", gotTarget)
	}
	targets := gotProject.Targets()
	found := false
	for _, target := range targets {
		if target.Name == "build" {
			found = true
			if strings.Join(target.RootTasks, ",") != "build" {
				t.Fatalf("unexpected synthetic target roots: %+v", target.RootTasks)
			}
		}
	}
	if !found {
		t.Fatalf("expected synthetic target for task build, got %+v", targets)
	}
}

func TestResolveExecutionProjectRejectsUnknownName(t *testing.T) {
	_, _, err := ResolveExecutionProject(resolveExecutionProject{}, "missing")
	if err == nil {
		t.Fatal("expected error for unknown target or task")
	}
}

func TestResolveExecutionProjectPreservesCacheNamespace(t *testing.T) {
	base := Define(func(_ context.Context, b *Builder) error {
		b.Name("demo")
		b.CacheNamespace("shared-build-cache")
		build := b.Task("build")
		b.Target("all", build)
		return nil
	})
	resolved, _, err := ResolveExecutionProject(base, "build")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := CacheNamespace(resolved), CacheNamespace(base); got != want {
		t.Fatalf("direct task uses cache namespace %q, declared target uses %q", got, want)
	}
}

type concurrentDetectionProject struct {
	resolveExecutionProject
	worktree string
}

func (concurrentDetectionProject) Name() string { return "concurrent-detection-test" }
func (p concurrentDetectionProject) DetectWorktree(worktree string) bool {
	return worktree == p.worktree
}

func TestDetectConcurrentRegistration(t *testing.T) {
	p := concurrentDetectionProject{worktree: t.TempDir()}
	registryMu.RLock()
	previous, existed := registry[p.Name()]
	registryMu.RUnlock()
	t.Cleanup(func() {
		registryMu.Lock()
		defer registryMu.Unlock()
		if existed {
			registry[p.Name()] = previous
		} else {
			delete(registry, p.Name())
		}
	})
	Register(p)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for range 1000 {
			Register(p)
		}
	}()
	defer workers.Wait()
	for range 1000 {
		detected, err := Detect(p.worktree)
		if err != nil {
			t.Fatal(err)
		}
		if detected.Name() != p.Name() {
			t.Fatalf("detected %q, want %q", detected.Name(), p.Name())
		}
	}
}
