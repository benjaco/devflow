package graph

import (
	"testing"

	"github.com/benjaco/devflow/pkg/project"
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

func TestAffectedByFilesRespectsRootAndDirRelativeIgnores(t *testing.T) {
	g, err := New(
		[]project.Task{
			{
				Name: "root_relative",
				Kind: project.KindOnce,
				Inputs: project.Inputs{
					Dirs:   []string{"internal/storage"},
					Ignore: []string{"internal/storage/sqlc"},
				},
			},
			{
				Name: "dir_relative",
				Kind: project.KindOnce,
				Inputs: project.Inputs{
					Dirs:   []string{"internal/storage"},
					Ignore: []string{"sqlc"},
				},
			},
			{
				Name: "source",
				Kind: project.KindOnce,
				Inputs: project.Inputs{
					Dirs: []string{"internal/storage"},
				},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("new graph: %v", err)
	}
	got := join(g.AffectedByFiles([]string{"internal/storage/sqlc/users.sql.go"}))
	if got != "source" {
		t.Fatalf("expected only non-ignored task affected, got %s", got)
	}
}

func TestExplainAffectedByFilesReportsMatchesAndIgnoredInputs(t *testing.T) {
	g, err := New(
		[]project.Task{
			{
				Name:   "codegen",
				Kind:   project.KindOnce,
				Inputs: project.Inputs{Files: []string{"schema.json"}},
			},
			{
				Name: "build",
				Kind: project.KindOnce,
				Inputs: project.Inputs{
					Dirs:   []string{"internal/storage"},
					Ignore: []string{"sqlc"},
				},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("new graph: %v", err)
	}
	impacts := g.ExplainAffectedByFiles([]string{"schema.json", "internal/storage/sqlc/users.sql.go", "internal/storage/repo.go"})
	if len(impacts) != 3 {
		t.Fatalf("unexpected impact count: %+v", impacts)
	}
	if impacts[0].File != "internal/storage/repo.go" || impacts[0].Task != "build" || !impacts[0].Affected || impacts[0].Reason != "dir" || impacts[0].Relative != "repo.go" {
		t.Fatalf("unexpected dir impact: %+v", impacts[0])
	}
	if impacts[1].File != "internal/storage/sqlc/users.sql.go" || impacts[1].Task != "build" || impacts[1].Affected || impacts[1].Reason != "ignored" || impacts[1].Ignore != "sqlc" {
		t.Fatalf("unexpected ignored impact: %+v", impacts[1])
	}
	if impacts[2].File != "schema.json" || impacts[2].Task != "codegen" || !impacts[2].Affected || impacts[2].Reason != "file" {
		t.Fatalf("unexpected file impact: %+v", impacts[2])
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
