# Adapter Guide

This is the adapter API guide for project owners who already understand the adoption flow.

If you are adding Devflow to a project for the first time, start with `devflow docs setup`. If you are changing Devflow itself, use `docs_contributors/README.md`.

Projects integrate with Devflow through the builder API in a project-local `devflow.project.go`.

Current runtime model:
- the project repo owns `./devflow.project.go`
- `devflow` compiles that file together with the core CLI into a worktree-local binary
- `devflow` then transfers execution into that local binary
- there is currently no built-in adapter fallback

Current first-version constraint:
- `devflow.project.go` must be self-contained
- use `package main`
- register the project in `init()`
- importing `github.com/benjaco/devflow/pkg/...` and standard library packages is supported
- arbitrary companion Go files from the project repo are not loaded yet

An adapter defines:
- tasks and services
- targets
- inputs and outputs
- instance ports/env
- optional managed database components

Tasks should stay semantic. The adapter decides which files, directories, env vars, and custom probes contribute to each fingerprint.

Minimal shape:

```go
package main

import (
    "context"

    "github.com/benjaco/devflow/pkg/project"
)

func init() {
    project.Register(project.Define(func(ctx context.Context, b *project.Builder) error {
        b.Name("my-project")
        b.DefaultTarget("up")

        build := b.Task("build").
            Command("go", "build", "./...").
            Inputs("go.mod", "go.sum", "cmd", "internal").
            Outputs("bin/my-project")

        b.Target("up", build)
        return nil
    }))
}
```

## Fuller Example

This example shows a realistic graph with generated SQL code, Prisma client generation, cached backend/frontend builds, a managed database, a fixed service readiness check, and a finite test target.

```go
package main

import (
    "context"
    "time"

    "github.com/benjaco/devflow/pkg/database"
    "github.com/benjaco/devflow/pkg/project"
)

func init() {
    project.Register(project.Define(func(ctx context.Context, b *project.Builder) error {
        b.Name("bikecoach")
        b.DefaultTarget("up")
        b.DotEnv(".env")
        b.RequiredCLIs("go", "npm", "npx", "sqlc", "docker")

        db := database.Postgres("prisma")
        prisma := database.Prisma("prisma").
            Schema("prisma/schema.prisma").
            MigrationDir("prisma/migrations").
            Database(db).
            CloneFromEnv("DEV_DATABASE_URL")

        sqlc := b.Task("sqlc").
            Command("sqlc", "generate").
            Inputs("sqlc.yaml", project.Glob("internal/storage/**/*.sql")).
            Outputs("internal/storage/sqlc")

        backend := b.Task("backend_build").
            Command("go", "build", "-o", "bin/coach", "./cmd/coach").
            DependsOn(sqlc, prisma.Client(b), prisma.Migrations(b)).
            Inputs("go.mod", "go.sum", "cmd", "internal", "prisma/schema.prisma").
            Outputs("bin/coach")

        frontend := b.Task("frontend_build").
            Command("npm", "run", "build").
            Inputs("package.json", "package-lock.json", "src", "vite.config.ts").
            Outputs("dist")

        app := b.Service("app").
            Command("bin/coach").
            DependsOn(backend, frontend).
            Inputs("bin/coach", "dist", ".env").
            Env("PORT", b.Port("app")).
            ReadyHTTP("app", "/health", 200).
            ReadyTimeout(30 * time.Second)

        unit := b.Task("unit").
            Command("go", "test", "./...").
            DependsOn(sqlc, prisma.Client(b)).
            Inputs("go.mod", "go.sum", "cmd", "internal", "prisma/schema.prisma").
            NoCache()

        b.Target("up", app)
        b.Target("build", backend, frontend)
        b.Target("test", unit)
        b.Target("new-migration", prisma.NewMigration(b))
        return nil
    }))
}
```

Important details:
- `sqlc` uses a glob, so generated Go files do not become inputs unless you declare them.
- `backend_build` depends on `sqlc`, Prisma client generation, and database migration state.
- `Outputs("bin/coach")` and `Outputs("dist")` make those finite tasks cacheable.
- Install/setup tasks such as `npm_install` should use `Stamp()` with local outputs like `node_modules` when they must run once per lockfile key without copying dependency folders into Devflow's global cache.
- `unit.NoCache()` keeps tests as a live check even though they have declared inputs.
- `database.Postgres("prisma")` defaults the snapshot directory; set `SnapshotRoot(...)` only when the default is wrong.

## Required CLI Installation

Adapters expose required command-line tools through `b.RequiredCLIs(...)` and `b.RequiredCLI(...)`.

The required CLI catalog is project-wide. Builder command tasks automatically select a catalog entry when the command name matches it. Add explicit `RequiredCLIs(...)` on a task only when the task needs a tool that is not the command binary.

Example:

```go
b.RequiredCLI(project.RequiredCLI{
    Name:    "sqlc",
    Command: "sqlc",
    Install: map[string]project.InstallScript{
        "darwin": {Script: "brew install sqlc"},
        "linux":  {Script: "go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest"},
    },
})
b.RequiredCLIs("go")

codegen := b.Task("codegen").
    Command("sqlc", "generate").
    Inputs("sqlc.yaml", project.Glob("internal/storage/**/*.sql")).
    Outputs("internal/storage/sqlc")

app := b.Service("app").
    Command("go", "run", "./cmd/app").
    DependsOn(codegen).
    Env("PORT", b.Port("app")).
    ReadyHTTP("app", "/health", 200)

b.Target("up", app)
```

That enables:
- `devflow clis status --project my-project`
- `devflow clis status --target up --project my-project`
- `devflow clis install --project my-project`
- `devflow clis install --target up --project my-project`
- `devflow doctor --target up --json`

Rules:
- `Command` should be the binary name Devflow can verify on `PATH`
- `RequiredCLIs` entries can use either required CLI `Name` or `Command`
- `doctor --target <target>` and `clis ... --target <target>` only check required CLIs attached to the selected target and its task closure
- install scripts should be platform-specific and idempotent when practical
- installers should leave the command actually available on `PATH`, because Devflow re-checks the command after install

## Dotenv Loading

Adapters can load `.env` files through the builder.

Example:

```go
b.DotEnv(".env")
b.Env("DEVFLOW_PROJECT", "my-project")
```

### Runtime Env, Tests, And Secrets

`InstanceConfig.Env` becomes the runtime environment for tasks and is persisted under `.devflow/state`. That makes runtime recovery and detached supervision deterministic, but it also means adapters should treat this env as local state, not as a secret vault.

Guidance:
- keep `.devflow/` ignored by git
- avoid placing long-lived production secrets in `InstanceConfig.Env`
- prefer short-lived local credentials for managed local services
- do not print full env maps in task logs
- if a task runs unit tests, override runtime service variables that would change test behavior

Example:

```go
return rt.RunCmdSpec(ctx, process.CommandSpec{
    Name: "go",
    Args: []string{"test", "./..."},
    Env: rt.EnvWith(map[string]string{
        "PORT": "8080",
    }),
})
```

This is important when a service target leases a dynamic runtime `PORT`, but unit tests expect a fixed port or no app server port at all.

Recommended precedence:
- `.env`
- adapter defaults
- devflow-owned runtime overrides

Use dotenv values for normal app configuration, but keep leased ports, instance IDs, and per-instance DB URLs under devflow control.

## Watch Restart Policies

Watch mode maps changed files to task inputs and then cascades through the selected target's task graph.

For service tasks, choose the narrowest restart policy that matches the service behavior:
- `project.RestartNever`: do not restart the service from watch file changes
- `project.RestartOnInputChange`: restart only when the service is in the affected downstream slice
- `project.RestartAlways`: restart on every watch cycle that has at least one directly affected task in the selected target

Dependency barriers still apply. If a changed upstream path reaches a task that is blocked from watch execution, downstream tasks past that blocked task do not run against stale outputs.

Use `WatchRestartOnServiceDeps` only when a service-to-service dependency should propagate restarts, such as a runtime service depending on a database service. Service-to-service restart propagation is off by default.

## Go Debug Services

Use `b.GoDebugService(...)` when Devflow should supervise Delve and the debuggee:

```go
codegen := b.Task("codegen").
    Command("go", "run", "./tools/codegen").
    Inputs("schema.json").
    Outputs("internal/generated")

apiDebug := b.GoDebugService("api_debug").
    Package("./cmd/api").
    BuildFlags("-tags=dev").
    BuildEnv("CGO_ENABLED", "1").
    DebugPort("api_debug").
    Env("PORT", b.Port("api")).
    Args("--config", ".devflow/dev.yaml").
    Inputs("go.mod", "go.sum", "cmd/api", "internal", "internal/generated").
    DependsOn(codegen).
    ReadyHTTP("api", "/health", 200).
    ReadyTimeout(30 * time.Second)

b.Target("debug", apiDebug)
```

The build step is intentionally external to Delve: Devflow runs `go build -gcflags=all=-N -l -o .devflow/debug/<task> <package>`, then launches `dlv exec` in headless multi-client mode on a stable local debug port. `status --json` includes the node's debug host, port, binary path, package, and an attach shape for editor integration.

Debug services are service-like for lifecycle purposes: they are supervised, never globally cached, can depend on normal build/codegen/database tasks, participate in watch restarts, and are checked by `flush` like other services. Use `ReadyHTTP` or another app readiness hook when the debuggee also needs to prove the application is usable after Delve starts.

Low-level adapters that still return raw `[]project.Task` should use `project.GoDebugService(...)` with `project.GoDebugServiceOptions` instead of hand-writing `go build` and `dlv exec` calls. `EnvPorts` maps runtime env vars such as `PORT` to named Devflow ports, while normal instance env such as `DATABASE_URL` is inherited automatically. Raw adapters must still include `go` and `dlv` in their project `RequiredCLIs()` catalog so target-scoped `doctor` can resolve those task requirements.

## DB Source Policies

When a DB snapshot miss happens, the adapter should rebuild from a configured base source instead of implying a reset command.

For Prisma/Postgres, the high-level component can clone from a remote development database URL only when it needs to rebuild the local base:

```go
prisma := database.Prisma("prisma").
    Schema("prisma/schema.prisma").
    MigrationDir("prisma/migrations").
    Database(database.Postgres("prisma")).
    CloneFromEnv("DEV_DATABASE_URL")
```

Semantics:
- Devflow first tries exact or nearest-prefix snapshot restore
- if no compatible snapshot exists, it recreates the local volume
- if `DEV_DATABASE_URL` is set, it clones that remote database into the local runtime
- then Devflow replays only the remaining migrations and snapshots the result

For lower-level custom flows, use a `database.SourcePolicy`. For Postgres databases that can be cloned through host `pg_dump`/`psql`, use the built-in remote source policy:

```go
policy := database.PostgresDumpSourcePolicy{
    PolicyName: "clone-dev",
    RemoteURL:  os.Getenv("DEV_DATABASE_URL"),
}
```

The policy writes the dump to a temporary file and only invokes `psql` after `pg_dump` succeeds, so a `pg_dump` version mismatch is surfaced as a Devflow task failure instead of being masked by an empty successful restore. In practice, use a host `pg_dump` whose major version matches the remote Postgres server.

It is not a "reset DB" operator action. The goal is to reuse the best compatible local state or rebuild a new base automatically.

## Managed Local Postgres Pattern

For application targets, use `database.Postgres(...)` unless you need a custom container lifecycle. It allocates a Devflow port, persists the local database identity in instance state, sets standard Postgres env vars, and defaults snapshots to `.devflow/db-snapshots`.

Recommended shape:

```go
db := database.Postgres("prisma")
```

The underlying runtime uses host-visible readiness, not only in-container readiness. The app connects through the mapped host port, so Devflow waits until Postgres is ready inside Docker and the host port accepts connections.

`EnsureRuntime` preserves the data volume, but recreates a stale container when its published host port does not match the current Devflow instance. Avoid unconditional container removal in normal startup paths.

For a Prisma project, the high-level path is:

```go
prisma := database.Prisma("prisma").
    Schema("prisma/schema.prisma").
    MigrationDir("prisma/migrations").
    Database(db).
    CloneFromEnv("DEV_DATABASE_URL")

app := b.Service("app").
    Command("npx", "tsx", "src/server.ts").
    DependsOn(prisma.Client(b), prisma.Migrations(b)).
    Env("PORT", b.Port("app")).
    ReadyHTTP("app", "/health", 200)

b.Target("up", app)
b.Target("new-migration", prisma.NewMigration(b))
```

The component task names are derived from the component name: `prisma_client`, `prisma_migrations`, and `prisma_new_migration` for `database.Prisma("prisma")`. Target names are consumer-owned. These docs use `new-migration`; you can expose an additional alias such as `migration_new` if an existing workflow already uses that name.

`prisma.Migrations(b)` will:
- inspect the Prisma schema and migration folder
- ignore non-directory entries in `prisma/migrations`, including Prisma's `migration_lock.toml`
- restore the nearest compatible cached migration point
- clone/rebuild a base database when no compatible snapshot exists
- ensure the host-visible Postgres runtime is ready
- run Prisma migrations through `npx prisma migrate deploy`
- snapshot the final state by default, plus intermediate prefixes only around migration folders with uncommitted Git changes when Git can identify them

That prefix snapshotting matters during development. Committed migrations are assumed to be stable and are not given partial cache points by default. If you edit or add an uncommitted migration, Devflow can restore the previous compatible prefix and apply only the changed local tail instead of rebuilding from the remote/base source. If Git is unavailable, the default behavior is final-only snapshotting. Devflow intentionally avoids snapshotting every historical Prisma prefix by default because that requires repeated Prisma deploy runs on cold rebuilds; use a custom `MigrateEach` only if you need exhaustive per-prefix snapshots.

`prisma.NewMigration(b)` uses the same prefix restore model before it invokes Prisma migration generation. It restores or rebuilds the managed database to the best compatible state, reapplies any missing or edited tail migrations, then runs `npx prisma migrate dev --name "$DEVFLOW_MIGRATION_NAME" --create-only`. This avoids Prisma seeing an old live database where an edited migration was already applied with different contents.

When a daemon-owned run is active, `devflow tui` has a `d` database/Prisma panel that shows the managed Postgres identity, recent cached Prisma migration-prefix snapshots, and schema/migration drift. Pressing `m` is an explicit migration-authoring action; it sends a daemon action that runs the project migration target such as `new-migration`, then relaunches the previously detached target so services restart through the graph. Normal `up` startup should still avoid hidden migration generation. `F2` and `F4` are backup keys for terminals where letter shortcuts conflict.

If `schema.prisma` declares models but no migrations exist, or if `schema.prisma` changes but no new migration appears, the Prisma migration task returns an explicit migration-needed error instead of pretending the database is current. Devflow records that task as `migration_needed` so the TUI can show an authoring action instead of a generic failure.

Custom migration guards can get the same task state by returning an error that implements `MigrationNeeded() bool`. Devflow also recognizes the built-in Prisma "generate one with GeneratePrismaMigration" guard text for compatibility.

For a plain SQL migration folder, use the generic migration workflow and an apply function:

```go
result, err := mgr.EnsureMigratedDatabase(ctx, database.ManagedMigrationOptions{
    Worktree:      worktree,
    DB:            db,
    MigrationsDir: "db/migrations",
    SourcePolicy:  policy,
    ApplyEach:     database.PostgresMigrationFileApplier("migration.sql"),
})
```

`ApplyEach` snapshots every migration prefix. If the latest migration changes, Devflow can restore the previous prefix snapshot and apply only the changed tail.

Keep migration generation as an explicit target/action, not part of normal `up` startup. The component registers Prisma config for the TUI, so `devflow tui` can flag drift, ask for a migration name, and run the same target-backed generation path without guessing paths. The TUI action is for authoring only; normal watch/flush still waits for a migration to exist instead of creating one implicitly.

Typical graph shape:
- `prisma_client`: finite task that runs `npx prisma generate`
- `prisma_migrations`: finite task that restores/rebuilds the local DB and applies migrations
- `prisma_new_migration`: explicit authoring task, not part of normal `up`
- `app`: service task depending on `prisma_client` and `prisma_migrations`

When validating a finite target that has service dependencies, prefer:

```bash
devflow run test --ci --json
```

`run --ci` starts service dependencies as readiness probes and stops them before returning. Plain attached `run` keeps services alive and is better for interactive development.

`devflow stop --all` stops the managed Postgres container recorded on the instance while preserving its Docker volume. Snapshot/restore paths may still rebuild or replace the volume when they explicitly own that work.

## Interactive Task Policy

Adapters should prefer non-interactive subprocesses.

Rules:
- for package installs and similar setup commands, prefer explicit confirmation flags such as `-y`, `--yes`, `--force`, or `CI=1` when the adapter has already decided the action is correct
- do not rely on hidden stdin prompts during normal `run` or `watch` targets
- if a task needs a real user choice, model it as an explicit command, target, or future TUI action instead of letting the process stall

This is especially important for detached runs and watch mode, where a blocked prompt is easy to miss and hard to recover from.

When a task truly does need prompt/answer interaction, the adapter can use interactive command specs instead of raw shell blocking:

```go
return rt.RunCmdSpec(ctx, process.CommandSpec{
    Name:        "my-tool",
    Args:        []string{"setup"},
    Interactive: true,
    Prompts: []process.PromptSpec{
        {Pattern: "Continue? [y/N]: ", Prompt: "Continue?", Kind: process.PromptConfirm},
        {Pattern: "Name: ", Prompt: "Name", Kind: process.PromptText},
    },
})
```

Semantics:
- Devflow watches subprocess output for the declared prompt patterns
- matching prompts become `interaction_requested` events
- detached runs can then be answered from the TUI
- the answer is written back to subprocess stdin and recorded as `interaction_answered`

This path should still be the exception, not the default adapter style.

### Prisma Guidance

Treat Prisma authoring and reset flows separately from normal startup.

Recommended split:
- normal DB prep:
  - restore the nearest DB snapshot
  - replay the remaining known migrations
- new migration authoring:
  - explicit action with a provided name
  - use `prisma migrate dev --name <name>` or `prisma migrate dev --name <name> --create-only`
- destructive reset:
  - explicit action only
  - use `prisma migrate reset --force`

Do not make normal boot targets depend on interactive `prisma migrate dev`. From Devflow's point of view, that command can still become non-deterministic when Prisma detects drift or wants a reset decision.

## Built Binary Tools

For repo-local helper executables, define the built binary as a normal output-producing builder task.

Example:

```go
tool := b.Task("build_auth_mapping").
    Command("go", "build", "-o", "bin/auth-mapping", "./tools/auth-mapping").
    Inputs("tools/auth-mapping", "go.mod", "go.sum").
    Outputs("bin/auth-mapping")

authMapping := b.Task("auth_mapping").
    Command("bin/auth-mapping", "--config", "backend/auth/config.json").
    DependsOn(tool).
    Inputs("backend/auth/config.json").
    Outputs("backend/auth/generated")
```

Rules:
- output paths should be real project artifact paths such as `bin/...`, `dist`, or generated source dirs
- do not put normal cache outputs under `.devflow/` unless the project truly treats that path as the artifact location
- keep outputs outside the input directories you fingerprint, or ignore them explicitly
- use task `Inputs(...)` to describe what should invalidate the build

Watch mode uses these declared inputs to decide which project paths to poll. Keep inputs narrow enough to cover real source changes without pulling in dependency trees such as `node_modules`.

For generated files, prefer narrow source globs. For example, `project.Glob("internal/storage/**/*.sql")` lets sqlc depend on query files without making `internal/storage/sqlc/*.go` upstream inputs.

Use this command when tuning watch inputs:

```bash
devflow graph affected --files internal/storage/sqlc/users.sql.go --explain --json
```

This gives you a standard way to compile helper binaries once, cache them by input hash, and run the restored artifact later from downstream tasks.

### Filtered Inputs

Some tools only care about a slice of each input file. For example, a Swagger generator may only need Go `@...` comment annotations plus public struct declarations, not every function body edit in the package.

Use filtered inputs to keep the watch scope explicit while narrowing the cache key:

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

Filtered inputs:
- still declare which files belong to the task, so watch and `graph affected` can see changes
- hash only the bytes returned by the filter when computing the task key
- skip files whose filtered content is empty
- include the filter signature in the task definition, so changing the filter changes the key

Built-in filters include:
- `project.LinesStartingWith("prefix")` for plain text files
- `project.GoCommentLinesStartingWith("@")` for Go comments after removing `//`, `/*`, and leading `*`
- `project.GoStructDeclarations()` for Go struct declarations plus their leading doc comments
- `project.CombineContentFilters(...)` for composing multiple filters

This is preferred over a full cache-key override when the normal file/env/dependency model is still correct and only the per-file content needs to be narrowed.

## Service Readiness

Service tasks can define an optional readiness function.

Builder shape:

```go
app := b.Service("app").
    Command("go", "run", "./cmd/app").
    Env("PORT", b.Port("app")).
    ReadyHTTP("app", "/health", 200).
    ReadyTimeout(30 * time.Second)
```

Use readiness when process start is not the same as service usability.

Examples:
- wait for a TCP listener on a named port
- poll an HTTP health endpoint
- wait for a ready file or socket to appear
- combine multiple checks with `ReadyAll(...)`

Rules:
- readiness should be narrow and deterministic
- a readiness check should describe service usability, not broad system health
- if a readiness check is configured, the engine will fail the task if it times out or the process exits first
- tasks without a readiness check are considered ready immediately after process start

## Local Stamps

Use stamped tasks for finite commands that mutate local development state and are expensive or noisy to rerun, but whose outputs should not be stored in the shared task cache.

```go
npmInstall := b.Task("npm_install").
    Command("npm", "install").
    Inputs("package.json", "package-lock.json").
    Outputs("node_modules").
    Stamp()
```

Stamped tasks:
- use the same input/dependency fingerprint model as cacheable tasks
- write a small per-worktree completion stamp under `.devflow/state`
- skip when the key still matches and declared local outputs still exist
- rerun when inputs change, dependency keys change, the task definition changes, or local outputs are missing
- never consult `<os.UserCacheDir()>/devflow/cache` to decide whether to skip
- do not copy or restore outputs from `<os.UserCacheDir()>/devflow/cache`

This is intended for package installs and similar local setup steps. Two worktrees with the same lockfile still need separate local install state, so each worktree gets its own stamp and local outputs. Prefer `NoCache()` for tests, migration authoring, and commands that should always execute when scheduled.

## Cache Key Overrides

By default, Devflow computes cache keys automatically from the task definition, selected inputs, env, and dependency keys.

For tasks with a better domain-specific notion of identity, the design allows a per-task cache-key override. This is intended for cases where the adapter can compute a more correct semantic key than generic file/env hashing.

Current shape:

```go
type CacheKeyFunc func(ctx context.Context, rt *Runtime) (string, error)

type Task struct {
    Name             string
    Kind             Kind
    Cache            bool
    CacheKeyOverride CacheKeyFunc
    // ...
}
```

Rules:
- only cacheable `KindOnce` tasks may use it
- the override replaces the automatic key body
- the engine should still salt the final key with engine version and task name
- override mode should be explicit and rare

Use it when:
- the task has a canonical artifact fingerprint already
- file hashing is too broad or too noisy
- correctness depends on semantic inputs that are easier to compute directly than to enumerate generically

Avoid it when:
- the generic input model is already sufficient
- the override would hide important dependency/config/version changes

If an override is used, the adapter owns correctness for that task’s cache identity.

The core engine does not know about Prisma, sqlc, Next.js, or any repo-specific conventions.
