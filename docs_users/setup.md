# Devflow Setup Docs

Use this when adding Devflow to a project or reshaping a project pipeline.

For daily command usage, TUI operation, watch/flush loops, logs, and status checks, use:

```bash
devflow docs development
```

## Prerequisites

Install Go first. Devflow needs Go because project graph definitions are Go code.

```bash
go install github.com/benjaco/devflow/cmd/devflow@latest
devflow version
devflow docs setup
```

Make sure `$(go env GOPATH)/bin` is on `PATH`. If `devflow upgrade` succeeds but `devflow version` does not change, run `which -a devflow`; another binary or symlink earlier on `PATH` is shadowing the Go-installed binary.

Update later with:

```bash
devflow upgrade
```

For testing a freshly pushed commit before the public Go proxy catches up, use `devflow upgrade --direct`.

## Setup Mental Model

Devflow runs a Go-defined development graph that lives in the project repo as `devflow.project.go`.

The project owns:
- task names and commands
- target names such as `up`, `test`, or `fullstack`
- file inputs that trigger cache invalidation and watch reruns
- service readiness checks
- required CLI/tool requirements
- runtime env layering

Devflow owns:
- graph scheduling
- cache restore/snapshot mechanics
- process supervision
- detached watch mode
- flush readiness coordination
- instance state, logs, ports, and JSON surfaces

Keep project-specific behavior in `devflow.project.go`. Do not add project-specific paths or framework assumptions to Devflow core packages.

## Add Devflow To A Repo

1. Add `.devflow/` to `.gitignore`.
2. Add `devflow.project.go` at the project root.
3. Define one small target first, usually `up` or `test`.
4. Run `devflow graph list --json`.
5. Run `devflow run <target> --json`.
6. Add inputs, outputs, and service readiness once the basic graph works.

Minimal `devflow.project.go`:

```go
package main

import (
	"context"

	"github.com/benjaco/devflow/pkg/project"
)

func init() {
	project.Register(project.Define(func(ctx context.Context, b *project.Builder) error {
		b.Name("my-app")
		b.DefaultTarget("up")
		b.RequiredCLIs("go")

		check := b.Task("check").
			Command("go", "version")

		b.Target("up", check)
		return nil
	}))
}
```

Replace the `check` command with a real project command once the bootstrap works.

## Design The Graph

Start with a graph that matches how developers already think about the repo.

Common task kinds:
- once task: finite command such as build, codegen, lint, or test
- service task: long-running process supervised by Devflow
- group task: grouping node for a target shape

Common target shapes:
- `test`: checks that should finish
- `up`: local development services
- `fullstack`: all services and required prep tasks
- `codegen`: generated artifacts only

Example target:

```go
b.Target("test", unitTest)
b.Target("up", apiDev, webDev)
```

## Cacheable Tasks

Only finite tasks can be cached. Service tasks are supervised, not cached.

A finite builder task becomes cacheable when it declares outputs:

```go
codegen := b.Task("codegen").
	Command("go", "run", "./tools/codegen").
	Inputs("schema.json").
	Outputs("internal/generated")
```

Prefer narrow semantic inputs over hashing the whole repository. Use `project.Glob("internal/storage/**/*.sql")` for generated-code inputs such as sqlc query files. Use `NoCache()` for finite commands with outputs that should not be restored from cache, such as explicit migration-authoring commands.

Task cache storage is global for the user:

```text
<os.UserCacheDir()>/devflow/cache/
```

Entries are namespaced by project. By default the namespace is `Project.Name()`. Override it only when you need a more stable or collision-resistant namespace:

```go
b.CacheNamespace("company-my-app")
```

## Services And Readiness

Service tasks start long-running processes:

```go
api := b.Service("api").
	Command("go", "run", "./cmd/api").
	DependsOn(codegen).
	Inputs("cmd/api", "internal/generated").
	Env("PORT", b.Port("api")).
	ReadyHTTP("api", "/health", 200)
```

Use readiness hooks for services that need a health check before downstream work or `flush` can report success. For named-port readiness, use `b.Port("api")` and pass that value into the service with `Env`.

## Prisma And Postgres

For a common Prisma/Postgres development graph, use the database components instead of hand-writing runtime and log plumbing:

```go
db := database.Postgres("prisma")

prisma := database.Prisma("prisma").
	Schema("prisma/schema.prisma").
	MigrationDir("prisma/migrations").
	Database(db).
	CloneFromEnv("DEV_DATABASE_URL")

app := b.Service("app").
	Command("npx", "tsx", "src/server.ts").
	DependsOn(prisma.Client(b), prisma.Migrations(b)).
	Inputs("src", "package.json", "package-lock.json", "prisma/schema.prisma").
	Env("PORT", b.Port("app")).
	ReadyHTTP("app", "/health", 200)

b.Target("up", app)
b.Target("new-migration", prisma.NewMigration(b))
```

`prisma.Migrations(b)` restores the best cached migration-prefix database state, applies only the missing tail, snapshots each prefix, and reports migration-needed states when `schema.prisma` and `prisma/migrations` are out of sync.

`prisma.NewMigration(b)` is an explicit authoring action. It reads `DEVFLOW_MIGRATION_NAME`, reconciles the managed database to the best compatible migration-prefix state, creates a Prisma migration, and is intentionally not task-cacheable. This keeps edited latest migrations usable: Devflow restores the prior prefix and reapplies the changed tail before Prisma authors the next migration.

The component task names are `prisma_client`, `prisma_migrations`, and `prisma_new_migration` when the component name is `prisma`. Target names are yours to choose. The docs use `new-migration`, but an adapter can also expose an alias such as `migration_new` while migrating from older scripts.

## Environment

Adapters can load `.env` files and then layer Devflow-managed values on top.

Recommended precedence:
1. `.env`
2. adapter defaults
3. Devflow-managed runtime values such as ports and DB URLs

With the builder API, use `b.DotEnv(".env")` and `b.Env(...)` instead of hand-rolling env parsing.

Runtime env is persisted under `.devflow/state` for detached runs, status, and relaunches. Keep `.devflow/` ignored, avoid storing long-lived production secrets in runtime env, and override service-specific values such as `PORT` for test commands when needed.

## Required CLIs

Expose required command-line tools through `b.RequiredCLIs(...)`. Commands created with the builder automatically select a matching required CLI catalog entry when one exists:

```go
b.RequiredCLIs("go")
b.RequiredCLI(project.RequiredCLI{
	Name:    "npm",
	Command: "npm",
	Install: map[string]project.InstallScript{
		"darwin": {Script: "brew install node"},
		"linux":  {Script: "sudo apt-get update && sudo apt-get install -y nodejs npm"},
	},
})

frontend := b.Task("frontend_build").Command("npm", "run", "build")
server := b.Service("server").Command("go", "run", "./cmd/server").DependsOn(frontend)
b.Target("up", server)
```

Then users can run:

```bash
devflow clis status --json
devflow clis status --target up --json
devflow doctor --target up --json
```

`devflow clis install --target up` runs platform-specific install scripts only for missing required CLIs, then re-checks that each installed command is available on `PATH`.

## What To Commit

Commit:
- `devflow.project.go`
- project files referenced by task inputs
- generated source only if your project normally commits it
- docs explaining your project targets

Do not commit:
- `.devflow/`
- local logs
- worktree-local binaries
- generated build modules under `.devflow/localbuild`
