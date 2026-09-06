package embeddedwebapp

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/planner"
	"github.com/benjaco/devflow/pkg/project"
)

func TestEmbeddedWebAppDeclaresFiniteVerification(t *testing.T) {
	p := embeddedWebAppProject{}
	g, err := graph.New(p.Tasks(), p.Targets())
	if err != nil {
		t.Fatal(err)
	}
	var verification []string
	for _, target := range p.Targets() {
		if target.Verification {
			verification = append(verification, target.Name)
		}
	}
	if !slices.Equal(verification, []string{"build-all"}) {
		t.Fatalf("verification targets = %v, want only build-all", verification)
	}
	closure, err := g.TargetClosure("build-all")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range closure {
		task := g.Tasks[name]
		if project.IsServiceKind(task.Kind) || task.Effects == nil {
			t.Fatalf("verification task %s lacks finite declared effects: %+v", name, task)
		}
	}
	for _, name := range []string{"build_tools", "build_coach", "build_frontend_main", "build_frontend_internal", "build_frontend_admin"} {
		if !slices.Contains(g.Tasks[name].Purposes, project.PurposeBuild) {
			t.Errorf("%s does not declare build purpose", name)
		}
	}
	if !slices.Contains(g.Tasks["sqlc_generate"].Purposes, project.PurposeGenerate) {
		t.Error("sqlc_generate does not declare generate purpose")
	}
	frontend := g.Tasks["build_frontend_main"].Effects
	if !slices.Contains(frontend.Writes, "frontend") || !slices.Contains(frontend.Writes, "internal/web/frontend") {
		t.Fatalf("npm install/build effects omit frontend writes or embedded assets: %+v", frontend)
	}
}

func TestEmbeddedWebAppPlansDeclaredVerificationWork(t *testing.T) {
	worktree := t.TempDir()
	if err := SeedWorktree(worktree); err != nil {
		t.Fatal(err)
	}
	t.Chdir(worktree)
	// Planning reads declarations even when their external prerequisites are absent.
	t.Setenv("PATH", "")
	before := embeddedPlannerFiles(t, worktree)
	p := plannerOnlyEmbeddedProject{t: t}
	for _, tt := range []struct {
		name   string
		file   string
		direct []string
		checks []string
		config bool
	}{
		{"frontend", "frontend/src/main.tsx", []string{"build_frontend_main"}, []string{"build_coach", "build_frontend_main"}, false},
		{"admin frontend", "frontend-admin/src/main.tsx", []string{"build_frontend_admin"}, []string{"build_coach", "build_frontend_admin"}, false},
		{"backend", "cmd/coach/main.go", []string{"build_coach"}, []string{"build_coach"}, false},
		{"shared modules", "go.mod", []string{"build_coach", "build_tools", "warmup_go_download"}, []string{"build_coach", "build_tools"}, false},
		{"entrypoint", "devflow.project.go", nil, []string{"build-all"}, true},
		{"deleted companion", "devflow_removed.go", nil, []string{"build-all"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, err := planner.Build(p, planner.Request{Files: []string{tt.file}, Intent: "verify"})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Advisory || result.ConfigurationChanged != tt.config {
				t.Fatalf("plan advisory=%t configurationChanged=%t, want true/%t", result.Advisory, result.ConfigurationChanged, tt.config)
			}
			// The existing parallel frontend builds share npm's cache. Declarations
			// report that overlap without assuming the external tool's locking works.
			if result.Resolved || len(result.Issues) != 0 || !slices.ContainsFunc(result.Conflicts, func(conflict planner.ResourceConflict) bool {
				return conflict.Resource == "npm-cache"
			}) {
				t.Fatalf("expected declared cache conflicts, got resolved=%t issues=%v conflicts=%v", result.Resolved, result.Issues, result.Conflicts)
			}
			var checks []string
			for _, check := range result.Checks {
				checks = append(checks, check.Name)
			}
			if !slices.Equal(checks, tt.checks) {
				t.Fatalf("checks = %v, want %v", checks, tt.checks)
			}
			if len(result.FileImpacts) != 1 || !slices.Equal(result.FileImpacts[0].DirectTasks, tt.direct) {
				t.Fatalf("file impact = %+v, want direct tasks %v", result.FileImpacts, tt.direct)
			}
			for _, excluded := range []string{"prepare_db_base", "prepare_db_runtime", "db_migrate", "snapshot_db_state", "postgres", "backend_dev"} {
				if slices.Contains(result.Closure, excluded) {
					t.Errorf("plan selected database/runtime task %s", excluded)
				}
			}
			if !slices.Equal(result.Prerequisites.CLIs, []string{"go", "npm", "sqlc"}) {
				t.Fatalf("declared prerequisites = %+v", result.Prerequisites)
			}
		})
	}
	if after := embeddedPlannerFiles(t, worktree); !reflect.DeepEqual(after, before) {
		t.Fatal("planning changed worktree files or created instance, dependency, or build outputs")
	}
}

type plannerOnlyEmbeddedProject struct {
	embeddedWebAppProject
	t *testing.T
}

func (p plannerOnlyEmbeddedProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	p.t.Fatal("planning invoked instance configuration")
	return project.InstanceConfig{}, nil
}

func (p plannerOnlyEmbeddedProject) Tasks() []project.Task {
	tasks := p.embeddedWebAppProject.Tasks()
	for i := range tasks {
		name := tasks[i].Name
		forbid := func(context.Context, *project.Runtime) error {
			p.t.Fatalf("planning invoked task %s instead of reading its declarations", name)
			return nil
		}
		if tasks[i].Run != nil {
			tasks[i].Run = forbid
		}
		if tasks[i].BeforeRun != nil {
			tasks[i].BeforeRun = forbid
		}
		if tasks[i].AfterReady != nil {
			tasks[i].AfterReady = forbid
		}
		if tasks[i].Ready != nil {
			tasks[i].Ready = project.ReadyFunc(forbid)
		}
	}
	return tasks
}

func embeddedPlannerFiles(t *testing.T, worktree string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	if err := filepath.WalkDir(worktree, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			files[path] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = string(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}
