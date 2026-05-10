package project

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
)

func TestBuilderDefinesProjectWithCachedOutputTask(t *testing.T) {
	p := Define(func(ctx context.Context, b *Builder) error {
		b.Name("demo")
		b.DefaultTarget("up")
		b.CacheNamespace("demo-cache")
		b.RequiredCLIs("sh")
		build := b.Task("build").
			Command("sh", "-c", "mkdir -p dist && printf ok > dist/app.txt").
			Inputs("package.json", Glob("sql/**/*.sql")).
			Outputs("dist")
		app := b.Service("app").
			Command("sh", "-c", "sleep 1").
			DependsOn(build).
			Env("PORT", b.Port("app")).
			ReadyTCP("app")
		b.Target("up", app)
		return nil
	})

	if p.Name() != "demo" {
		t.Fatalf("unexpected project name %q", p.Name())
	}
	if got := PreferredTarget(p); got != "up" {
		t.Fatalf("PreferredTarget = %q, want up", got)
	}
	if got := CacheNamespace(p); got != "demo-cache" {
		t.Fatalf("CacheNamespace = %q, want demo-cache", got)
	}
	tasks := p.Tasks()
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	build := taskByNameForTest(tasks, "build")
	if !build.Cache {
		t.Fatal("builder outputs should make once tasks cacheable")
	}
	if len(build.Inputs.Paths) != 1 || build.Inputs.Paths[0] != "package.json" {
		t.Fatalf("unexpected path inputs: %+v", build.Inputs.Paths)
	}
	if len(build.Inputs.Globs) != 1 || build.Inputs.Globs[0] != "sql/**/*.sql" {
		t.Fatalf("unexpected glob inputs: %+v", build.Inputs.Globs)
	}
	if len(build.Outputs.Paths) != 1 || build.Outputs.Paths[0] != "dist" {
		t.Fatalf("unexpected output paths: %+v", build.Outputs.Paths)
	}
	if len(build.RequiredCLIs) != 1 || build.RequiredCLIs[0] != "sh" {
		t.Fatalf("expected command required CLI to be selected, got %+v", build.RequiredCLIs)
	}
}

func TestBuilderStampedTaskDoesNotEnableCacheForOutputs(t *testing.T) {
	p := Define(func(ctx context.Context, b *Builder) error {
		b.Name("demo")
		install := b.Task("npm_install").
			Command("npm", "install").
			Inputs("package.json", "package-lock.json").
			Outputs("node_modules").
			Stamp()
		b.Target("up", install)
		return nil
	})

	install := taskByNameForTest(p.Tasks(), "npm_install")
	if install.Cache {
		t.Fatal("stamped task must not become globally cacheable")
	}
	if !install.Stamp {
		t.Fatal("expected stamped task")
	}
	if len(install.Outputs.Paths) != 1 || install.Outputs.Paths[0] != "node_modules" {
		t.Fatalf("unexpected outputs: %+v", install.Outputs)
	}
}

func TestBuilderSupportsFilteredInputs(t *testing.T) {
	filter := CombineContentFilters(GoCommentLinesStartingWith("@"), GoStructDeclarations())
	p := Define(func(ctx context.Context, b *Builder) error {
		b.Name("demo")
		swagger := b.Task("swagger").
			Command("swag", "init").
			Inputs(Filtered(Glob("internal/**/*.go"), filter)).
			Outputs("docs")
		b.Target("docs", swagger)
		return nil
	})

	task := taskByNameForTest(p.Tasks(), "swagger")
	if len(task.Inputs.Filtered) != 1 {
		t.Fatalf("expected one filtered input, got %+v", task.Inputs.Filtered)
	}
	input := task.Inputs.Filtered[0]
	if input.Path != "internal/**/*.go" {
		t.Fatalf("unexpected filtered input path %q", input.Path)
	}
	if input.Filter.Signature == "" {
		t.Fatal("expected filtered input signature")
	}
	if !task.Cache {
		t.Fatal("filtered input task with outputs should be cacheable")
	}
}

func TestBuilderCommandEnvCanUsePortRef(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	worktree := t.TempDir()
	p := Define(func(ctx context.Context, b *Builder) error {
		b.Name("demo")
		b.RequiredCLIs("sh")
		task := b.Task("write_port").
			Command("sh", "-c", "printf %s \"$PORT\" > port.txt").
			Env("PORT", b.Port("app")).
			Outputs("port.txt")
		b.Target("test", task)
		return nil
	})

	task := taskByNameForTest(p.Tasks(), "write_port")
	rt := &Runtime{
		Worktree: worktree,
		Instance: &api.Instance{
			ID:    "inst",
			Ports: map[string]int{"app": 4567},
		},
		Env: map[string]string{},
	}
	if err := task.Run(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "port.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "4567" {
		t.Fatalf("port env = %q, want 4567", string(data))
	}
}

func TestBuilderConfigureInstanceLoadsDotEnvAndPorts(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, ".env"), []byte("FROM_DOTENV=yes\nNODE_ENV=from-dotenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Define(func(ctx context.Context, b *Builder) error {
		b.Name("demo")
		b.DotEnv(".env")
		b.Env("NODE_ENV", "development")
		b.Port("app")
		task := b.Task("noop").Run(func(context.Context, *Runtime) error { return nil })
		b.Target("test", task)
		return nil
	})
	cfg, err := p.ConfigureInstance(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.PortNames) != 1 || cfg.PortNames[0] != "app" {
		t.Fatalf("unexpected port names: %+v", cfg.PortNames)
	}
	if cfg.Env["FROM_DOTENV"] != "yes" {
		t.Fatalf("dotenv env not loaded: %+v", cfg.Env)
	}
	if cfg.Env["NODE_ENV"] != "development" {
		t.Fatalf("builder env should override dotenv, got %q", cfg.Env["NODE_ENV"])
	}
}

func taskByNameForTest(tasks []Task, name string) Task {
	for _, task := range tasks {
		if task.Name == name {
			return task
		}
	}
	panic("missing task " + name)
}
