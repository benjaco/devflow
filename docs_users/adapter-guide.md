# Adapter Guide

This is the adapter API guide for project owners who already understand the adoption flow.

If you are adding Devflow to a project for the first time, start with `devflow docs setup`. If you are changing Devflow itself, use `docs_contributors/README.md`.

Projects integrate with Devflow through the builder API in project-root Go adapter sources.

Current runtime model:

- the project repo owns `./devflow.project.go`
- `devflow.project.go` is the mandatory marker and entrypoint
- regular root-level files matching `devflow_*.go` are optional companion sources
- every `devflow_*_test.go` file is excluded from the runtime build and remains an ordinary Go test
- `devflow` compiles the discovered adapter sources together with the core CLI into a worktree-local binary
- `devflow` then transfers execution into that local binary
- there is currently no built-in adapter fallback

Adapter source contract:

- use `package main` in the entrypoint and every companion
- register the project in `init()`
- importing `github.com/benjaco/devflow/pkg/...` and standard library packages is supported
- companions are opt-in by the exact `devflow_*.go` filename convention and must stay beside the entrypoint at the worktree root
- nested files, arbitrary sibling Go files, symlinks, directories, and special files are not loaded
- existing adapters containing only `devflow.project.go` continue unchanged

Recommended larger-adapter layout:

```text
devflow.project.go       # small required entrypoint and project registration
devflow_shared.go        # shared constants, environment, helpers
devflow_frontend.go      # frontend tasks and services
devflow_backend.go       # backend and database tasks
devflow_ci.go            # CI, deployment, and E2E targets
devflow_watch_test.go    # normal Go test; excluded from runtime bootstrap
```

Filenames participate in the local CLI content key. Adding, removing, renaming, or editing a companion rebuilds the CLI; timestamp-only changes do not. Changes to excluded tests or unrelated root Go files also do not rebuild it.

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
        b.RequiredCLIs("go", "npm", "npx", "sqlc")

        db := database.Postgres("prisma")
        prisma := database.Prisma("prisma").
            Schema("prisma/schema.prisma").
            MigrationDir("prisma/migrations").
            Database(db).
            CloneFromEnvContainerized("DEV_DATABASE_URL")

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

        prisma.NewMigration(b)

        b.Target("up", app)
        b.Target("build", backend, frontend)
        b.Target("test", unit)
        return nil
    }))
}
```

Important details:
- `sqlc` uses a glob, so generated Go files do not become inputs unless you declare them.
- `backend_build` depends on `sqlc`, Prisma client generation, and database migration state.
- `Outputs("bin/coach")` and `Outputs("dist")` make those finite tasks cacheable.
- Cached tasks must declare at least one output. `OutputFiles` requires regular files and `OutputDirs` requires real directories; paths must remain inside the worktree without symlink parents. Duplicate declarations and children of an already declared output directory are supported. Restores stage all artifacts before replacing outputs and roll back failed publication; an unrecoverable rollback error identifies the retained backups.
- Install/setup tasks such as `npm_install` should use `Stamp()` with local outputs like `node_modules` when they must run once per lockfile key without copying dependency folders into Devflow's global cache.
- `unit.NoCache()` keeps tests as a live check even though they have declared inputs.
- `database.Postgres("prisma")` defaults the snapshot directory; set `SnapshotRoot(...)` only when the default is wrong.
- `CloneFromEnvContainerized(...)` uses the clients bundled in the managed Postgres image, so this example needs a reachable Docker Engine but not host `pg_dump`/`psql` binaries.

## Commands That Must Produce Files

Some generators incorrectly exit with code zero before producing their files. Use `project.CommandOutputTasklet` when command success must converge on specific filesystem outputs:

```go
generate := project.CommandOutputTasklet{
    Command: process.CommandSpec{
        Name: "npm",
        Args: []string{"run", "generate"},
    },
    RequiredFiles: []string{
        "src/generated/payload-types.ts",
        "src/app/**/importMap.js",
    },
    MaxAttempts: 5,
    RetryDelay:  250 * time.Millisecond,
}

generated := b.Task("payload_codegen").
    Inputs("package.json", "package-lock.json", "src/payload.config.ts").
    Outputs("src/generated", "src/app/(payload)/admin").
    Run(generate.Run)
```

Every `RequiredFiles` path or Devflow glob must match at least one regular file. The command always runs once. After a zero exit, the tasklet checks the files, waits for the retry delay, checks again for delayed writes, and only then reruns the command. A non-zero exit fails immediately. Defaults are five attempts and a 250 ms delay.

Use `RequireNewFiles: true` when pre-existing matches must not satisfy the run. This is useful for authoring commands that must add a file to an existing folder.

For fully generated directories, cleanup is opt-in:

```go
generate.OutputDirs = []string{"src/generated", "src/app/(payload)/admin"}
generate.OutputHashFiles = []string{".cache/payload-types.hash", ".cache/import-map.hash"}
generate.CleanOutputDirs = true
generate.RequireNewFiles = true
```

Cleanup happens once before the first attempt, not between retries, so a partially successful attempt can still converge. `OutputHashFiles` lists exact hash/marker files maintained by the generator; they are removed in the same cleanup phase whenever `CleanOutputDirs` removes the outputs and cannot be configured independently. Devflow validates all directory and hash targets before deleting anything, then removes hashes first so a directory-removal failure cannot leave a stale hash claiming the output is current.

Paths must be worktree-relative; every cleaned directory must contain at least one required pattern, and cleanup rejects escaping paths, globs, Git/Devflow state paths, non-file hash targets, and symlink traversal. Only enable it for disposable generated directories and their corresponding hashes. `CommandOutputTasklet` verifies runtime behavior but does not declare graph outputs automatically, so keep using `Outputs`, `OutputFiles`, or `OutputDirs` on the task builder as appropriate.

## PayloadCMS/Postgres Component

PayloadCMS projects can use the same builder style:

```go
db := database.Postgres("payload").PortName("postgres")

payload := database.PayloadCMS("payload").
    Config("src/payload.config.ts").
    MigrationDir("src/migrations").
    SchemaInputs("src/collections", "src/globals", "src/fields").
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
payload.ConfigureDevService(app)

b.Target("up", app)
```

The Payload config must let Devflow control development schema push:

```ts
db: postgresAdapter({
    pool: { connectionString: process.env.DATABASE_URL! },
    push: process.env.PAYLOAD_SCHEMA_PUSH === 'true',
})
```

`ConfigureDevService(app)` fingerprints the Payload config, configured schema inputs, package manifest/common lockfiles, and a password-free database identity before every service start. It sets `PAYLOAD_SCHEMA_PUSH=true` on the first start or after one of those inputs changes, and `false` for unrelated restarts. The applied fingerprint is written under the worktree's per-instance `.devflow/state` only after the service passes its declared readiness check. A configured service must therefore define `Ready`, `ReadyHTTP`, `ReadyTCP`, or `ReadyFile`.

The same schema inputs are direct service inputs, so watch mode restarts the app with `true` when a collection/global/field/config module changes. Once that restart is ready, later non-schema restarts return to `false`. The default schema module paths are `src/collections`, `src/globals`, and `src/fields`; configure `SchemaInputs(...)` or `AddSchemaInputs(...)` for blocks, plugins, or reusable Payload config modules elsewhere. The default package inputs cover `package.json`, `package-lock.json`, `pnpm-lock.yaml`, `yarn.lock`, `bun.lock`, and `bun.lockb`; use `PackageInputs(...)` for another workspace layout. Passwords and the full `DATABASE_URL` are never stored in the schema state.

By default, the component uses `npx payload migrate` and `npx payload migrate:create <name>`. If your project wraps Payload in an npm script, use:

```go
payload.Command("npm", "run", "payload", "--")
```

Payload migration creation may ask for confirmations around destructive changes. The component declares those prompts so the TUI and daemon can ask the user. It also requires a newly created regular file under the configured migration directory: a zero exit without a new migration is retried instead of reported as success. Existing migrations are preserved; the component never enables output-directory cleanup for migration authoring. Normal boot/watch targets should depend on `payload.Migrations(b)`, not `payload.NewMigration(b)`.

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

Declare required environment inputs separately from CLI tools:

```go
b.RequiredEnv("SHARED_API_TOKEN")

deploy := b.Task("deploy").
    RequiredEnv("DEPLOY_TOKEN").
    Command("go", "run", "./cmd/deploy")
```

Project-wide requirements apply to every doctor scope. Task requirements apply only when that task is in the selected target closure. `devflow doctor --target deploy --strict --json` reports each required key with `set` and `source`, emits the complete JSON even when a key is missing, and exits nonzero on failed checks. `InputEnv(...)` still controls fingerprints/watch semantics; use `RequiredEnv(...)` when absence itself should be diagnosed before execution. Prisma `CloneFromEnv(...)` and `CloneFromEnvContainerized(...)` add their source URL key automatically.

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
- persisted instance values as a recovery baseline
- `.env` and adapter defaults
- explicitly set invoking-process/CI values for project-declared env keys
- devflow-owned runtime overrides

Use dotenv values for normal app configuration, but let CI/shell values override defaults. Devflow selects only keys referenced by project env, task `InputEnv`/`RequiredEnv`, or project-wide `RequiredEnv` rather than persisting the caller's entire environment. Leased ports, instance IDs, and per-instance DB URLs remain under devflow control and win last.

## Explicit Interactive Actions

Prompting commands should be explicit authoring or operator actions, not hidden in normal `up`/watch paths. For custom tools, use interactive command specs:

```go
task := b.Task("dangerous_authoring_action").
    CommandSpec(process.CommandSpec{
        Name:        "mytool",
        Args:        []string{"author"},
        Interactive: true,
        Prompts: []process.PromptSpec{
            {
                Patterns: []string{
                    "Drop field? [y/N]: ",
                    "Delete column? [y/N]: ",
                },
                Prompt: "Accept destructive migration warning?",
                Kind:   process.PromptConfirm,
                Repeat: true,
            },
        },
    }).
    NoCache()
```

Use `Patterns` when a tool has multiple possible prompt texts. Use `Repeat` when the same spec should answer a sequence of similar confirmations.

To make that task a foreground user action instead of a normal target, register it with an action spec:

```go
b.Action("custom.migration.create").
    Kind("devflow.database.migration.create").
    Category(project.ActionCategoryAuthoring).
    Component("custom").
    Task(task).
    Input(project.ActionInput{
        Name:       "name",
        Required:   true,
        Positional: true,
        Env:        "MIGRATION_NAME",
    }).
    Writes("migrations/**").
    RelaunchPreviousTargetAfterSuccess()
```

Users can then run `devflow action run custom.migration.create --name add_status` or, for migration-create actions, `devflow migration create add_status --component custom`.

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

The policy writes the dump to an owner-only temporary file and only invokes `psql` after `pg_dump` succeeds, so a `pg_dump` version mismatch is surfaced as a Devflow task failure instead of being masked by an empty successful restore. It executes both clients directly rather than through `sh`, keeping this path portable on macOS, Linux, and Windows. `CloneFromEnv(...)` adds `pg_dump` and `psql` to target-scoped required-CLI checks. In practice, use a host `pg_dump` whose major version matches the remote Postgres server; on Apple Silicon Homebrew installs the clients with `brew install libpq`, and `$(brew --prefix libpq)/bin` may need to be added to `PATH`.

Both host commands receive password-free URLs plus PostgreSQL connection variables and a temporary owner-only `PGPASSFILE`; passwords do not appear in process arguments, and inherited PostgreSQL URL variables are sanitized. A URL may supply its password in userinfo or as `?password=...`; the query password takes precedence, is percent-decoded for pgpass, and every `password` parameter is removed while options such as `sslmode`, `application_name`, and timeouts remain. Devflow rejects `sslpassword`, `oauth_client_secret`, and `passfile` query parameters because it cannot move those credential channels through this transport without exposing them. Rejection errors do not reproduce the credential. To avoid installing host libpq clients, select the clients bundled in the managed Postgres image:

```go
prisma := database.Prisma("prisma").
    Database(database.Postgres("prisma")).
    CloneFromEnvContainerized("DEV_DATABASE_URL")
```

The equivalent low-level policy sets `ClientStrategy: database.PostgresClientContainer`. It sends pgpass records only over Docker exec stdin and puts password-free URLs in exec metadata. This usually makes a running Docker Desktop/Engine sufficient. The remote hostname must be reachable from inside the managed container; a source URL pointing at host `localhost` needs a container-reachable hostname such as the platform's host gateway. Its `pg_dump` major follows the selected managed image, so choose an image compatible with the remote server or use the host-client strategy when another client version is required.

It is not a "reset DB" operator action. The goal is to reuse the best compatible local state or rebuild a new base automatically.

## Managed Local Postgres Pattern

For application targets, use `database.Postgres(...)` unless you need a custom container lifecycle. It allocates a Devflow port, persists the local database identity in instance state, sets standard Postgres env vars, and defaults snapshots to `.devflow/db-snapshots`.

Recommended shape:

```go
db := database.Postgres("prisma")
```

For a spatial database, use the PostGIS flavor with the same component API:

```go
db := database.PostGIS("geo", 16)
```

The second argument is the PostgreSQL major version; supported values are `16`, `17`, and `18`. The PostGIS flavor is architecture-aware. On Docker amd64 it pulls `postgis/postgis:16-3.5`, `postgis/postgis:17-3.5`, or `postgis/postgis:18-3.6`. On Docker arm64/aarch64, where those upstream images are unavailable, Devflow builds and caches a version-specific image from `postgres:<major>-bookworm` with Debian's matching `postgresql-<major>-postgis-3` packages. Subsequent starts reuse the local image. Devflow enables `CREATE EXTENSION IF NOT EXISTS postgis` in the instance database as part of readiness, so a PostGIS runtime is not reported ready until spatial SQL is usable. A custom `Image(...)` still takes precedence when an adapter deliberately supplies its own compatible image, but the required version argument must match that image.

Each PostGIS major uses a distinct `-pg<major>` Docker volume. PostgreSQL 16 and 17 mount it at `/var/lib/postgresql/data`; PostgreSQL 18 mounts it at `/var/lib/postgresql`, matching the official image's changed persistent-data layout. Changing the component from one major to another therefore creates a fresh local cluster instead of attempting an unsafe in-place major upgrade. Move data between major versions with `pg_dump`/`pg_restore` or an explicit `pg_upgrade` workflow.

The underlying runtime uses host-visible readiness, not only in-container readiness. The app connects through the mapped host port, so Devflow waits until Postgres is ready inside Docker and the host port accepts connections.

Low-level adapters that declare a raw Postgres service task should supervise the managed container directly:

```go
manager := database.New()
handle, err := manager.StartRuntimeService(ctx, rt.Instance.DB, database.RuntimeServiceOptions{
    OnLine: rt.LineEmitter(),
})
if err != nil {
    return err
}
rt.RegisterServiceHandle(handle)
return nil
```

The handle has no host PID because Docker owns the container process. Devflow still follows its logs, detects exit, checks it during `flush`, and stops it during watch restarts, CI cleanup, and normal shutdown. Do not add a `docker info` prerequisite or launch `docker logs -f` as a wrapper service; the database package connects to the Docker Engine directly and supports Unix sockets, Windows named pipes, and configured TCP/TLS or SSH contexts.

A custom `ServiceHandle.Stop()` must join cleanup and return an error if it cannot stop the resource; after successful stop, `Alive()` must return false. Devflow preserves failed handles and blocks replacement until cleanup is confirmed. This applies to handles registered by finite tasks as well as services. `Runtime.RegisterServiceHandle` uses the same `OnServiceHandle` callback for OS processes and PID-less managed resources. Keep `ConfigureInstance` declarative: resources that need cleanup belong in task execution and must register a handle. The worktree execution lease protects cooperating Devflow operations; adapters still need explicit isolation for external resources shared by different worktrees.

Custom low-level migration tasks can read a SQL file and call `database.New().ExecSQL(ctx, rt.Instance.DB, sql)`. This runs `psql` inside the managed container through the Engine exec API and returns its output even on failure, so adapters can forward it with `Runtime.EmitLogLine` without depending on `docker exec` or host path syntax.

`EnsureRuntime` preserves the data volume, but recreates a stale container when its published host/container port mapping or resolved Postgres image does not match the current Devflow instance. Avoid unconditional container removal in normal startup paths. The default `postgres:16.14` runtime and `alpine:3.24.1` snapshot sidecar are official multi-architecture images, so Docker selects `linux/arm64` natively on Apple Silicon. Custom images must publish the architecture they are expected to run on; adapters should not force `linux/amd64` unless emulation is an intentional project requirement. The high-level component exposes `Image(...)`, `SidecarImage(...)`, and `ContainerPort(...)` for compatible custom runtimes.

Snapshot archives contain physical Postgres cluster files. Their manifests record the source image platform and configured PostgreSQL major; managed migration restore ignores manifests with missing required metadata or a different architecture/version, rebuilding from source/migrations instead. This intentionally invalidates old Intel snapshot caches after a move to Apple Silicon and prevents physical clusters from crossing PostgreSQL majors. Project data that must move between machines or versions should use a logical `pg_dump`, not `.devflow/db-snapshots` as a transport format.

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
prisma.NewMigration(b)
```

The component task names are derived from the component name: `prisma_client`, `prisma_migrations`, and `prisma_new_migration` for `database.Prisma("prisma")`. `prisma.NewMigration(b)` registers an explicit action with kind `devflow.database.migration.create`; it is not a normal target and does not run during `up`, `watch`, or `test`.

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

When a daemon-owned run is active, `devflow tui` has a `d` database/Prisma panel that shows the managed Postgres identity, recent cached Prisma migration-prefix snapshots, and schema/migration drift. Pressing `m` is an explicit migration-authoring action; it sends a daemon action of kind `devflow.database.migration.create`, then relaunches the previously detached target so services restart through the graph. Normal `up` startup should still avoid hidden migration generation. `F2` and `F4` are backup keys for terminals where letter shortcuts conflict.

If `schema.prisma` declares models but no migrations exist, or if `schema.prisma` changes but no new migration appears, the Prisma migration task returns an explicit migration-needed error instead of pretending the database is current. Devflow records that task as `migration_needed` so the TUI can show an authoring action instead of a generic failure.

Custom migration guards get the same task state by returning an error that implements `MigrationNeeded() bool` and returns true. Wrapped errors retain that signal. Ordinary error messages remain failures regardless of their wording, so diagnostic text cannot accidentally offer migration actions for an unrelated problem.

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

Keep migration generation as an explicit action, not part of normal `up` startup. The component registers Prisma config and a migration-create action for the TUI, so `devflow tui` can flag drift, ask for a migration name, and run the action without guessing paths. The TUI action is for authoring only; normal watch/flush still waits for a migration to exist instead of creating one implicitly.

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

Declared outputs also identify task-owned writes during watch execution. Devflow records their metadata when the producer finishes, before finite-task cache persistence, so later edits to an input/output file remain eligible for rebuilding while downstream work runs. Keep output scopes narrow: declaring a whole source tree as output assigns that tree to the producer during its execution. Flush observes declared inputs; `watch_restart_required` means restart/warmup policy prevented required work from refreshing.

### Validation Mode

Use the finite validation runner while tuning an adapter:

```bash
devflow validate build --mode artifacts --details issues --json
devflow validate build --mode orders --details issues --max-orders 1000 --json
```

JSON validation defaults to `issues`, which preserves exact counts and returns bounded actionable samples without listing every successful path. `summary` keeps only state/count/timing/resource information; `full` explicitly restores exhaustive path arrays. `--max-listed-paths` defaults to 200 per issue category, with a separate overall text bound. Live phase/counter progress is written to stderr and the final JSON remains the only stdout document.

Artifact mode projects the filesystem separately for every task. Explicit `Inputs(...)`, file/dir/glob/filtered inputs, and normal ignore rules select source files. Declared outputs from every transitive dependency are also materialized, so a consumer does not need to duplicate its producer's output path as a file input merely to receive the dependency artifact. Only declared outputs are archived for downstream tasks.

Both validation modes use the same `BeforeRun` then optional `Run` sequence as normal execution. A finite task can do all its work in `BeforeRun`. A hook failure prevents `Run`; hook logs and errors appear in the task's failure evidence. Declare the files a hook reads and writes just as you would for `Run`. Hook-provided values in `rt.Env` reach that task's `Run`, while siblings retain their original environment. This clones the task runtime value and its environment map only: keep task-scoped changes in `rt.Env`, and avoid mutating shared `rt.Instance.Env` or adapter globals.

After both callbacks return, or a hook fails, artifact validation compares filesystem snapshots. A final changed file outside `Outputs(...)`, `OutputFiles(...)`, or `OutputDirs(...)` is an `undeclared_output`; a missing or wrong-kind declaration is a `missing_output`. If the task cannot run in the projected worktree, it is reported as `task_failed_with_projected_inputs`. That failure can still be an ordinary command or hook failure, so inspect its captured log before assuming the absent declaration is the only cause.

Order mode starts each permutation with all ordinary worktree source files, but removes `.git`, `.devflow`, and declared generated outputs. It runs each topological order sequentially with caches and stamps bypassed. Producer outputs that are also that producer's inputs are restored for in-place transformations. All permutations must produce the same final declared-output snapshot.

Adapter rules exposed by validation:

- a target closure must be finite; services and debug services are not permutation-testable
- validation does not run service readiness or `AfterReady`; prompts from hooks or `Run` fail immediately
- a finite task that registers supervised handles fails validation; Devflow attempts to stop every handle, including after hook failure, and reports stop failures alongside the original task error
- different tasks must not own overlapping output paths
- input/output declarations must be worktree-relative and cannot point into `.git` or `.devflow`
- the worktree root cannot be an output
- safe relative symlinks are preserved when their resolved target stays inside the source projection; absolute and external symlinks are rejected
- projected/cache copies are bounded by entry and byte limits, so unexpectedly large trees fail with a path-specific error instead of expanding indefinitely
- the validation-wide budget spans every copy, snapshot, and output-transfer phase; JSON `metrics` reports cumulative files/logical bytes and peak temporary storage, while `resourceFailure` reports limit/disk-reserve exhaustion before writable cleanup
- dependency artifacts are transferred between the active sandbox and validation-owned holding directories rather than copied into a second expanded tree; no writable hardlinks touch the source or persistent cache
- `Runtime.Mode` is `api.ModeValidation`, with `DEVFLOW_VALIDATION=1` and `DEVFLOW_VALIDATION_MODE` set for commands that need safe validation-specific behavior

The sandbox covers worktree-relative filesystem access. It does not virtualize databases, network APIs, absolute paths, global tool caches, or background processes that a task starts without registering. Keep validation targets finite and externally safe to repeat. Artifact success proves explicit worktree input sufficiency for that run; it does not prove the declared input set is minimal.

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
- if a readiness check is configured, the engine will fail the task if it times out or the supervised service exits first
- tasks without a readiness check are considered ready immediately after registering their service handle

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
