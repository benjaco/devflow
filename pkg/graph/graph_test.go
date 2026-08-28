package graph

import (
	"context"
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

func TestValidateStampedTaskRules(t *testing.T) {
	if _, err := New(
		[]project.Task{{Name: "serve", Kind: project.KindService, Stamp: true}},
		[]project.Target{{Name: "up", RootTasks: []string{"serve"}}},
	); err == nil {
		t.Fatal("expected service stamp validation error")
	}
	if _, err := New(
		[]project.Task{{Name: "install", Kind: project.KindOnce, Cache: true, Stamp: true}},
		[]project.Target{{Name: "up", RootTasks: []string{"install"}}},
	); err == nil {
		t.Fatal("expected cache and stamp validation error")
	}
}

func TestValidateRejectsAfterReadyOnFiniteTask(t *testing.T) {
	_, err := New(
		[]project.Task{{
			Name:       "generate",
			Kind:       project.KindOnce,
			AfterReady: func(context.Context, *project.Runtime) error { return nil },
		}},
		[]project.Target{{Name: "up", RootTasks: []string{"generate"}}},
	)
	if err == nil {
		t.Fatal("expected finite-task AfterReady validation error")
	}
}

func TestValidateRejectsAfterReadyWithoutReadiness(t *testing.T) {
	_, err := New(
		[]project.Task{{
			Name:       "serve",
			Kind:       project.KindService,
			AfterReady: func(context.Context, *project.Runtime) error { return nil },
		}},
		[]project.Target{{Name: "up", RootTasks: []string{"serve"}}},
	)
	if err == nil {
		t.Fatal("expected AfterReady readiness validation error")
	}
}

func TestValidateRejectsBeforeRunOnGroup(t *testing.T) {
	_, err := New(
		[]project.Task{{
			Name:      "all",
			Kind:      project.KindGroup,
			BeforeRun: func(context.Context, *project.Runtime) error { return nil },
		}},
		[]project.Target{{Name: "up", RootTasks: []string{"all"}}},
	)
	if err == nil {
		t.Fatal("expected group BeforeRun validation error")
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

func TestExplainAffectedByFilesReportsPathAndGlobInputs(t *testing.T) {
	g, err := New([]project.Task{
		{
			Name: "sqlc",
			Kind: project.KindOnce,
			Inputs: project.Inputs{
				Paths: []string{"sqlc.yaml"},
				Globs: []string{"internal/storage/**/*.sql"},
			},
		},
	}, []project.Target{{Name: "test", RootTasks: []string{"sqlc"}}})
	if err != nil {
		t.Fatal(err)
	}
	impacts := g.ExplainAffectedByFiles([]string{"sqlc.yaml", "internal/storage/users.sql", "internal/storage/users.go"})
	if len(impacts) != 2 {
		t.Fatalf("expected 2 impacts, got %+v", impacts)
	}
	if impacts[0].Reason != "glob" && impacts[1].Reason != "glob" {
		t.Fatalf("expected one glob impact, got %+v", impacts)
	}
}

func TestExplainAffectedByFilesReportsFilteredInputs(t *testing.T) {
	g, err := New([]project.Task{
		{
			Name: "swagger",
			Kind: project.KindOnce,
			Inputs: project.Inputs{
				Filtered: []project.FilteredInput{
					project.Filtered(project.Glob("internal/**/*.go"), project.GoStructDeclarations()),
				},
				Ignore: []string{"internal/generated"},
			},
		},
	}, []project.Target{{Name: "docs", RootTasks: []string{"swagger"}}})
	if err != nil {
		t.Fatal(err)
	}
	impacts := g.ExplainAffectedByFiles([]string{"internal/api/users.go", "internal/generated/users.go"})
	if len(impacts) != 2 {
		t.Fatalf("expected 2 impacts, got %+v", impacts)
	}
	if impacts[0].File != "internal/api/users.go" || impacts[0].Reason != "filtered_glob" || !impacts[0].Affected {
		t.Fatalf("unexpected filtered impact: %+v", impacts[0])
	}
	if impacts[1].File != "internal/generated/users.go" || impacts[1].Reason != "ignored" || impacts[1].Affected {
		t.Fatalf("unexpected ignored filtered impact: %+v", impacts[1])
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
