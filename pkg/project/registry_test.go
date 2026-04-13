package project

import (
	"context"
	"strings"
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
