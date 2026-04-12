package graph

import (
	"testing"

	"devflow/pkg/project"
)

func TestValidateDuplicateTaskFails(t *testing.T) {
	_, err := New(
		[]project.Task{
			{Name: "a", Kind: project.KindOnce},
			{Name: "a", Kind: project.KindOnce},
		},
		nil,
	)
	if err == nil {
		t.Fatal("expected duplicate task error")
	}
}

func TestTargetClosureAndClosures(t *testing.T) {
	g, err := New(
		[]project.Task{
			{Name: "a", Kind: project.KindOnce},
			{Name: "b", Kind: project.KindOnce, Deps: []string{"a"}},
			{Name: "c", Kind: project.KindOnce, Deps: []string{"b"}},
			{Name: "d", Kind: project.KindOnce, Deps: []string{"a"}},
		},
		[]project.Target{{Name: "main", RootTasks: []string{"c"}}},
	)
	if err != nil {
		t.Fatalf("new graph: %v", err)
	}

	closure, err := g.TargetClosure("main")
	if err != nil {
		t.Fatalf("target closure: %v", err)
	}
	if got, want := join(closure), "a,b,c"; got != want {
		t.Fatalf("closure=%s want=%s", got, want)
	}
	if got, want := join(g.Downstream([]string{"a"})), "a,b,c,d"; got != want {
		t.Fatalf("downstream=%s want=%s", got, want)
	}
	if got, want := join(g.Upstream([]string{"c"})), "a,b,c"; got != want {
		t.Fatalf("upstream=%s want=%s", got, want)
	}
}

func TestAffectedByFilesUsesPathBoundaries(t *testing.T) {
	g, err := New(
		[]project.Task{
			{Name: "backend", Kind: project.KindOnce, Inputs: project.Inputs{Dirs: []string{"backend"}}},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("new graph: %v", err)
	}
	if got := g.AffectedByFiles([]string{"backend/file.go"}); join(got) != "backend" {
		t.Fatalf("expected backend to be affected, got %v", got)
	}
	if got := g.AffectedByFiles([]string{"backend2/file.go"}); len(got) != 0 {
		t.Fatalf("unexpected affected tasks: %v", got)
	}
}

func join(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += ","
		}
		out += item
	}
	return out
}
