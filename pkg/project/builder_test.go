package project

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestBuilderGoDebugServiceDefinesDebugMetadata(t *testing.T) {
	p := Define(func(ctx context.Context, b *Builder) error {
		b.Name("debug-demo")
		gen := b.Task("generate").
			Run(func(context.Context, *Runtime) error { return nil }).
			Inputs("schema.sql")
		debug := b.GoDebugService("api_debug").
			Package("./cmd/api").
			BuildFlags("-tags=dev").
			BuildEnv("CGO_ENABLED", "1").
			DebugPort("debug_api").
			Env("PORT", b.Port("api")).
			Args("--config", ".devflow/dev.yaml").
			Inputs("cmd/api", "internal").
			DependsOn(gen).
			ReadyHTTP("api", "/health", 200)
		b.Target("debug", debug)
		return nil
	})

	task := taskByNameForTest(p.Tasks(), "api_debug")
	if task.Kind != KindDebugService {
		t.Fatalf("kind = %q, want %q", task.Kind, KindDebugService)
	}
	if task.Restart != RestartOnInputChange {
		t.Fatalf("restart = %q, want %q", task.Restart, RestartOnInputChange)
	}
	if task.Cache || task.Stamp {
		t.Fatalf("debug services must not be cache/stamp tasks: cache=%v stamp=%v", task.Cache, task.Stamp)
	}
	if task.Debug == nil {
		t.Fatal("expected debug metadata")
	}
	if task.Debug.Type != "go" || task.Debug.PortName != "debug_api" || task.Debug.Package != "./cmd/api" {
		t.Fatalf("unexpected debug metadata: %+v", task.Debug)
	}
	if !strings.Contains(task.Debug.Binary, ".devflow/debug/api_debug") {
		t.Fatalf("unexpected debug binary %q", task.Debug.Binary)
	}
	if got := strings.Join(task.RequiredCLIs, ","); got != "go,dlv" {
		t.Fatalf("required CLIs = %q, want go,dlv", got)
	}
	if len(task.Deps) != 1 || task.Deps[0] != "generate" {
		t.Fatalf("deps = %+v, want generate", task.Deps)
	}

	cfg, err := p.ConfigureInstance(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	portNames := strings.Join(cfg.PortNames, ",")
	if !strings.Contains(portNames, "debug_api") || !strings.Contains(portNames, "api") {
		t.Fatalf("expected app and debug ports, got %q", portNames)
	}
}

func TestGoDebugServiceHelperDefinesRawTask(t *testing.T) {
	task := GoDebugService("backend_debug", GoDebugServiceOptions{
		Package:       "./backend/src",
		BuildEnv:      map[string]string{"CGO_ENABLED": "0"},
		DebugPortName: "backend_debug",
		EnvPorts:      map[string]string{"PORT": "backend"},
		Deps:          []string{"backend_codegen", "postgres"},
		Inputs:        Inputs{Files: []string{"go.mod"}, Dirs: []string{"backend/src"}},
		Ready:         ReadyHTTPNamedPort("backend", "/health", 200),
		Description:   "Debug backend",
	})

	if task.Kind != KindDebugService {
		t.Fatalf("kind = %q, want %q", task.Kind, KindDebugService)
	}
	if task.Run == nil {
		t.Fatal("expected built-in debug runner")
	}
	if task.Debug == nil || task.Debug.PortName != "backend_debug" || task.Debug.Package != "./backend/src" {
		t.Fatalf("unexpected debug metadata: %+v", task.Debug)
	}
	if !strings.Contains(task.Debug.Binary, ".devflow/debug/backend_debug") {
		t.Fatalf("unexpected debug binary %q", task.Debug.Binary)
	}
	if got := strings.Join(task.RequiredCLIs, ","); got != "go,dlv" {
		t.Fatalf("required CLIs = %q, want go,dlv", got)
	}
	if len(task.Deps) != 2 || task.Deps[0] != "backend_codegen" || task.Deps[1] != "postgres" {
		t.Fatalf("deps = %+v", task.Deps)
	}
	if task.Restart != RestartOnInputChange {
		t.Fatalf("restart = %q, want %q", task.Restart, RestartOnInputChange)
	}
	if task.Signature == "" {
		t.Fatal("expected stable signature")
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
