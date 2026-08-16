# Devflow Setup Docs

Use this when adding Devflow to a project or reshaping a project pipeline.

For daily command usage, TUI operation, watch/flush loops, logs, and status checks, use:

```bash
devflow docs development
```

## Prerequisites

Install Go 1.26.6 or newer first. Devflow needs Go because project graph definitions are Go code.

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

The command reports its target immediately and streams `go install` download/build output while it runs. With `--json`, that live progress stays on stderr and stdout contains only the final JSON result.

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

When a tool only cares about part of a source file, use a filtered input instead of overriding the whole cache key. This keeps watch/debug paths explicit while hashing only the relevant content:

```go
swaggerRelevant := project.CombineContentFilters(
	project.GoCommentLinesStartingWith("@"),
	project.GoStructDeclarations(),
)

swagger := b.Task("swagger").
	Command("swag", "init", "-g", "cmd/api/main.go").
	Inputs(project.Filtered(project.Glob("internal/**/*.go"), swaggerRelevant)).
	Outputs("docs")
```

`GoStructDeclarations()` includes the doc comments immediately before each struct, so comments attached to API structs can invalidate generated docs. Function body edits that do not change `@` comments or structs do not change the filtered cache key.

Use `Stamp()` for local install/setup tasks that should run once per input key but should not copy heavyweight mutable folders into the global cache:

```go
npmInstall := b.Task("npm_install").
	Command("npm", "install").
	Inputs("package.json", "package-lock.json").
	Outputs("node_modules").
	Stamp()
```

`Stamp()` records successful completion in the current worktree state using the normal task fingerprint. It never checks or restores from the global task cache when deciding whether to skip. If the same input key is seen again in that worktree and declared local outputs still exist, Devflow marks the task done without running it. If `package-lock.json` changes, or that worktree's `node_modules` is removed, it runs again. This is the right shape for package-manager installs; use `NoCache()` for tests and authoring commands that should execute every time they are scheduled.

Task cache storage is global for the user:

```text
<os.UserCacheDir()>/devflow/cache/
```

Entries are namespaced by project. By default the namespace is `Project.Name()`. Override it only when you need a more stable or collision-resistant namespace:

```go
b.CacheNamespace("company-my-app")
```

For CI cache configuration, do not reconstruct OS-specific paths or target keys manually:

```bash
devflow cache path --json
devflow cache key --target build --json
```

The first command returns the global root and project namespace path. The second returns one aggregate target key plus the real per-task keys used by cacheable and stamped tasks.

## Validate Inputs, Outputs, And Task Ordering

After a finite build/codegen/test target works normally, use validation mode to check that its graph contract is strong enough for caching and future edits:

```bash
devflow validate build --mode artifacts --json
devflow validate build --mode orders --max-orders 1000 --json
```

Artifact mode runs each task in a disposable projected worktree. That worktree contains only the task's declared file inputs and declared outputs from upstream dependencies. It then checks that the task produced its declared outputs and did not leave other changed files behind. This catches common mistakes such as reading an undeclared config file, generating into a directory that was not listed in `Outputs(...)`, or forgetting an output entirely.

Order mode runs every dependency-valid task order, one task at a time, from the same clean source state. All orders must succeed and leave identical declared artifacts. If one order fails, the usual cause is a missing `DependsOn(...)` edge or an undeclared shared side effect. If two orders succeed but return different artifact digests, the tasks are order-dependent even though the DAG says they are independent.

Use both checks together with:

```bash
devflow validate build --mode all --json
```

The default exhaustive bound is 1000 orders. Devflow does not silently sample a larger graph: it returns `complete=false` without running permutations, and you can raise `--max-orders` deliberately. Validate finite targets only; service/debug-service targets are rejected because they do not finish one by one.

Validation copies ordinary worktree files into temporary sandboxes and never uses task cache hits or stamps. It deliberately omits `.git` and `.devflow`. Safe internal relative symlinks are preserved instead of dereferenced, while absolute or externally resolving symlinks are rejected; projection size is also bounded. It proves that declared worktree inputs are sufficient for the observed execution, but not that every declared input is necessary, and it cannot sandbox databases, networks, global tool caches, absolute paths, or unregistered background processes. Choose targets whose external effects are safe to repeat.

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

## Go Debug Services

Use `GoDebugService` when Devflow should own a Go service under Delve:

```go
apiDebug := b.GoDebugService("api_debug").
	Package("./cmd/api").
	BuildFlags("-tags=dev").
	BuildEnv("CGO_ENABLED", "1").
	DebugPort("api_debug").
	Env("PORT", b.Port("api")).
	Args("--config", ".devflow/dev.yaml").
	Inputs("go.mod", "go.sum", "cmd/api", "internal").
	DependsOn(codegen).
	ReadyHTTP("api", "/health", 200).
	ReadyTimeout(30 * time.Second)

b.Target("debug", apiDebug)
```

Devflow builds a stable worktree-local debug binary with `go build -gcflags=all=-N -l`, then supervises:

```bash
dlv exec .devflow/debug/api_debug --headless --api-version=2 --listen=127.0.0.1:<port> --accept-multiclient --continue -- <args>
```

The debug port is a named local port. `status --json` includes each debug node's host, port, binary, package, and an editor attach shape. Use a VS Code/Cursor Go remote attach configuration that points at that stable host/port.

Debug services are long-running service tasks, not cacheable tasks. On watch changes, Devflow stops the Delve process tree, rebuilds the debug binary, and relaunches Delve on the same named port before marking the service ready. Use `DependsOn(...)` for generated code, database prep, or other build steps that must complete before the debug binary is rebuilt.

`GoDebugService` automatically marks `go` and `dlv` as required CLIs for the task, so `doctor --target debug --json` reports whether Delve is installed.

On macOS, Delve also needs the normal Apple developer-tools permissions. If a debug service reports `stub exited while waiting for connection`, check `DevToolsSecurity -status` and follow Delve's macOS installation guidance; Devflow does not enable machine-wide developer security settings for you.

Adapters that still return raw `[]project.Task` can use the same built-in handler without writing Delve lifecycle code:

```go
func (myProject) RequiredCLIs() []project.RequiredCLI {
	return []project.RequiredCLI{
		{Name: "go", Command: "go"},
		{Name: "dlv", Command: "dlv"},
	}
}

project.GoDebugService("api_debug", project.GoDebugServiceOptions{
	Package:       "./cmd/api",
	DebugPortName: "api_debug",
	EnvPorts:      map[string]string{"PORT": "api"},
	Deps:          []string{"codegen", "db"},
	Inputs:        project.Inputs{Files: []string{"go.mod", "go.sum"}, Dirs: []string{"cmd/api", "internal"}},
	Ready:         project.ReadyHTTPNamedPort("api", "/health", 200),
})
```

## Prisma And Postgres

Managed Postgres requires a running Docker Engine. Devflow uses the official Engine Go API directly; the `docker` command does not need to be installed or listed with `RequiredCLIs`. It honors `DOCKER_HOST`, `DOCKER_CONTEXT`, the active context in Docker's config, and then the platform default. On macOS and Windows, install and start Docker Desktop before running the target. Windows uses Docker's native named-pipe transport and must be in Linux-container mode for the Postgres images. Apple Silicon is supported natively: Devflow's default `postgres:16.14` and `alpine:3.24.1` images publish `linux/arm64` variants, and Devflow deliberately does not force `linux/amd64` emulation. If an adapter overrides either image through `Image(...)` or `SidecarImage(...)`, choose a multi-architecture image or an explicit ARM64-compatible build; `ContainerPort(...)` is available for compatible custom Postgres images that listen somewhere other than 5432.

For projects that need PostGIS, pass the PostgreSQL major explicitly, for example `db := database.PostGIS("geo", 16)`. Versions 16, 17, and 18 are supported. On amd64 Devflow uses the matching maintained tag (`16-3.5`, `17-3.5`, or `18-3.6`) from `postgis/postgis`. On Apple Silicon it builds a native local image once from `postgres:<major>-bookworm` with the matching Debian PostGIS packages, then reuses it; it does not run an amd64 image through emulation. The first arm64 start for each major therefore takes longer and requires Docker build/network access. Database readiness also creates the `postgis` extension in the selected database and fails if spatial support is unavailable.

The PostgreSQL major is persisted with the instance and included in the local PostGIS volume name. Version 18 uses `/var/lib/postgresql`, while 16 and 17 use `/var/lib/postgresql/data`, following the official image layouts. Switching the argument starts with a separate empty volume; Devflow does not perform an implicit physical major-version upgrade. Use logical dumps or an explicit upgrade workflow when data must move between versions.

Database snapshots are physical Postgres volume archives, not portable SQL dumps. New manifests record the Docker image platform and configured PostgreSQL major. When moving a worktree from Intel to Apple Silicon or changing PostgreSQL majors, Devflow treats snapshots with missing/incompatible metadata as cache misses, then rebuilds from the configured source and migrations. Keep `DEV_DATABASE_URL` or another base source available when a cold rebuild depends on remote seed data.

For a common Prisma/Postgres development graph, use the database components instead of hand-writing runtime and log plumbing:

```go
db := database.Postgres("prisma")

prisma := database.Prisma("prisma").
	Schema("prisma/schema.prisma").
	MigrationDir("prisma/migrations").
	Database(db).
	CloneFromEnvContainerized("DEV_DATABASE_URL")

app := b.Service("app").
	Command("npx", "tsx", "src/server.ts").
	DependsOn(prisma.Client(b), prisma.Migrations(b)).
	Inputs("src", "package.json", "package-lock.json", "prisma/schema.prisma").
	Env("PORT", b.Port("app")).
	ReadyHTTP("app", "/health", 200)

b.Target("up", app)
prisma.NewMigration(b)
```

`prisma.Migrations(b)` restores the best cached migration-prefix database state, applies only the missing tail, snapshots the final state, and reports migration-needed states when `schema.prisma` and `prisma/migrations` are out of sync. When the migration folder is in a Git worktree, Devflow only adds intermediate prefix snapshots around migration folders with uncommitted Git changes, including newly added or edited local migrations. If Git is unavailable, Devflow falls back to the final snapshot only. This keeps committed history fast while preserving the local workflow where you edit an in-progress migration and need to restore the previous prefix.

`CloneFromEnvContainerized(...)` uses `pg_dump` and `psql` from the managed Postgres image, so a reachable Docker Engine is sufficient and host libpq clients are not required. Passwords are delivered through Docker exec stdin, not command arguments or inspectable exec environment. The remote database hostname must be reachable from inside the container; host `localhost` is not the container's localhost. The bundled client major follows the managed image and must be compatible with the remote server; select a compatible image or use host clients when another version is needed.

Use `CloneFromEnv(...)` when you deliberately want host clients. Devflow declares `pg_dump` and `psql` in target-scoped doctor output, invokes them directly without a Unix shell, and uses an owner-only temporary `PGPASSFILE` with password-free command URLs. On Apple Silicon with Homebrew, `brew install libpq` installs both; because the formula can be keg-only, add `$(brew --prefix libpq)/bin` to `PATH`. Match the `pg_dump` major version to the remote Postgres server.

`prisma.NewMigration(b)` registers an explicit authoring action. It reads the action input `name` through `DEVFLOW_MIGRATION_NAME`, reconciles the managed database to the best compatible migration-prefix state, creates a Prisma migration, and is intentionally not task-cacheable. This keeps edited latest migrations usable: Devflow restores the prior prefix and reapplies the changed tail before Prisma authors the next migration.

## PayloadCMS And Postgres

For PayloadCMS projects backed by Postgres, use the PayloadCMS database component so migration application and migration authoring are modeled consistently with the rest of the graph:

```go
db := database.Postgres("payload").PortName("postgres")

payload := database.PayloadCMS("payload").
	Config("src/payload.config.ts").
	MigrationDir("src/migrations").
	SchemaInputs("src/collections", "src/globals").
	Database(db)

npmInstall := b.Task("npm_install").
	Command("npm", "install").
	Inputs("package.json", "package-lock.json").
	Stamp()

migrations := payload.Migrations(b).DependsOn(npmInstall)
payload.NewMigration(b).DependsOn(npmInstall)

app := b.Service("app").
	Command("npm", "run", "dev").
	DependsOn(migrations).
	Inputs("src", "package.json", "package-lock.json").
	InputEnv("DATABASE_URL", "PAYLOAD_SECRET", "PORT").
	Env("PORT", b.Port("app")).
	ReadyHTTP("app", "/health", 200)

b.Target("up", app)
b.Target("setup", npmInstall, migrations)
```

`payload.Migrations(b)` starts the managed Postgres runtime when a `database.Postgres` component is attached, waits for host-port readiness, and runs Payload's normal migration apply command. It is not task-cacheable because database state is a live runtime side effect.

Payload schema inputs default to `src/collections` and `src/globals` in addition to the config and migration directory. Use `SchemaInputs(...)` when your collections, globals, blocks, or other config modules live elsewhere. Watch mode uses the same inputs, so edits to those modules are picked up and rerun the Payload migration preparation task.

`payload.NewMigration(b)` registers the explicit authoring action. It reads the action input `name` through `DEVFLOW_MIGRATION_NAME`, runs Payload migration creation, writes into the configured migration directory, and is intentionally not task-cacheable. Payload can prompt for confirmations when a migration may be destructive, for example after deleting a field. Devflow models those prompts as explicit interactive events, so the TUI or daemon client can ask the user instead of hiding a hanging subprocess in watch mode.

Payload has been observed returning success without writing a migration. Devflow therefore requires a newly created regular file under the configured migration directory, waits briefly for delayed filesystem writes, and retries a zero-exit/no-file command up to five attempts. A non-zero exit still fails immediately. Existing migration history is never deleted by this retry path.

By default the component runs:

```bash
npx payload migrate
npx payload migrate:create "$DEVFLOW_MIGRATION_NAME"
```

If your project uses a package-manager script instead, configure the command prefix:

```go
payload := database.PayloadCMS("payload").
	Command("npm", "run", "payload", "--")
```

Then Devflow runs `npm run payload -- migrate` and `npm run payload -- migrate:create <name>`. Use `DEVFLOW_PAYLOAD_FORCE_ACCEPT_WARNING=1` only when your adapter intentionally wants Payload's force-accept-warning path for automation; normal human/TUI flows should let the prompt surface.

The component task names are `prisma_client`, `prisma_migrations`, and `prisma_new_migration` when the component name is `prisma`. Migration creation is exposed as a first-class action with kind `devflow.database.migration.create`, not as a normal target.

## Environment

Adapters can load `.env` files and then layer Devflow-managed values on top.

Recommended precedence:
1. persisted instance values as a recovery baseline
2. `.env` and adapter defaults
3. explicitly set invoking-process/CI values for project-declared env keys
4. Devflow-managed runtime values such as ports and DB URLs

With the builder API, use `b.DotEnv(".env")` and `b.Env(...)` instead of hand-rolling env parsing. Process values are selected only for keys the project references through configured env, `InputEnv`, or `RequiredEnv`; Devflow does not persist the whole caller environment.

Runtime env is persisted under `.devflow/state` for daemon-owned runs, status, and relaunches. Keep `.devflow/` ignored, avoid storing long-lived production secrets in runtime env, and override service-specific values such as `PORT` for test commands when needed.

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

Declare required non-empty environment values with project- or task-level metadata:

```go
b.RequiredEnv("SHARED_API_TOKEN")
deploy.RequiredEnv("DEPLOY_TOKEN")
```

Then make prerequisite failures actionable in CI:

```bash
devflow doctor --target deploy --strict --json
```

The JSON result includes `requiredEnv` entries with their set state and source. `--strict` still writes that complete result, then exits nonzero when a required CLI or env value is missing. Prisma clone components automatically require the key passed to `CloneFromEnv(...)` or `CloneFromEnvContainerized(...)`.

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
