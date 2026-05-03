# Adapter Guide

This is the adapter API guide for project owners who already understand the adoption flow.

If you are adding Devflow to a project for the first time, start with `docs_users/README.md`. If you are changing Devflow itself, use `docs_contributors/README.md`.

Projects integrate with Devflow by implementing `pkg/project.Project` in a project-local `devflow.project.go`.

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
- tasks
- targets
- instance configuration

Tasks should stay semantic. The adapter decides which files, directories, env vars, and custom probes contribute to each fingerprint.

Minimal shape:

```go
package main

import (
    "context"

    "github.com/benjaco/devflow/pkg/project"
)

type myProject struct{}

func init() {
    project.Register(myProject{})
}

func (myProject) Name() string { return "my-project" }

func (myProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
    _ = ctx
    _ = worktree
    return project.InstanceConfig{Label: "my-project"}, nil
}

func (myProject) Tasks() []project.Task {
    return []project.Task{
        {
            Name: "build",
            Kind: project.KindOnce,
            Run: func(ctx context.Context, rt *project.Runtime) error {
                _ = ctx
                _ = rt
                return nil
            },
        },
    }
}

func (myProject) Targets() []project.Target {
    return []project.Target{
        {Name: "up", RootTasks: []string{"build"}},
    }
}
```

## Required CLI Installation

Adapters can expose required command-line tools through `RequiredCLIProvider`.

`RequiredCLIs()` is the project catalog. Attach catalog entries to tasks or targets with `RequiredCLIs` when different targets need different tools.

Example:

```go
func (myProject) RequiredCLIs() []project.RequiredCLI {
    return []project.RequiredCLI{
        {
            Name:    "sqlc",
            Command: "sqlc",
            Install: map[string]project.InstallScript{
                "darwin": {Script: "brew install sqlc"},
                "linux":  {Script: "go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest"},
            },
        },
    }
}

func (myProject) Tasks() []project.Task {
    return []project.Task{
        {
            Name:         "codegen",
            Kind:         project.KindOnce,
            RequiredCLIs: []string{"sqlc"},
            Run:          runCodegen,
        },
        {
            Name:         "app",
            Kind:         project.KindService,
            Deps:         []string{"codegen"},
            RequiredCLIs: []string{"go"},
            Run:          runApp,
        },
    }
}

func (myProject) Targets() []project.Target {
    return []project.Target{
        {
            Name:         "up",
            RootTasks:    []string{"app"},
            RequiredCLIs: []string{"docker"},
        },
    }
}
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

Adapters can now load `.env` files directly through `pkg/project`.

Example:

```go
dotenv, err := project.LoadOptionalDotEnvInWorktree(worktree, ".env")
if err != nil {
    return project.InstanceConfig{}, err
}

return project.InstanceConfig{
    Env: project.MergeEnvMaps(dotenv, map[string]string{
        "DEVFLOW_PROJECT": "my-project",
    }),
    Finalize: func(inst *api.Instance) error {
        inst.Env = project.MergeEnvMaps(inst.Env, map[string]string{
            "DATABASE_URL": computedDatabaseURL,
        })
        return nil
    },
}
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
    Env: project.MergeEnvMaps(rt.Env, map[string]string{
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

## DB Source Policies

When a DB snapshot miss happens, the adapter should rebuild from a configured base source instead of implying a reset command.

Current shape:

```go
policy := database.CommandSourcePolicy{
    PolicyName: "clone-dev",
    Spec: process.CommandSpec{
        Name: "sh",
        Args: []string{"-c", "./scripts/clone-dev.sh"},
    },
}

prepared, err := database.New().PreparePrismaBase(ctx, inst.DB, state, policy, database.PrepareOptions{
    Worktree: worktree,
    Env:      env,
})
```

Semantics:
- Devflow first tries exact or nearest-prefix snapshot restore
- if no compatible snapshot exists, it recreates the local volume
- if a source policy is configured, it starts a temporary Postgres runtime and applies that policy
- then the adapter can replay only the remaining migrations and snapshot the result

This is the right abstraction for:
- clone-from-dev scripts today
- local bootstrap/startup scripts later

For Postgres databases that can be cloned through host `pg_dump`/`psql`, use the built-in remote source policy:

```go
policy := database.PostgresDumpSourcePolicy{
    PolicyName: "clone-dev",
    RemoteURL:  os.Getenv("DEV_DATABASE_URL"),
}
```

The policy writes the dump to a temporary file and only invokes `psql` after `pg_dump` succeeds, so a `pg_dump` version mismatch is surfaced as a Devflow task failure instead of being masked by an empty successful restore. In practice, use a host `pg_dump` whose major version matches the remote Postgres server.

It is not a "reset DB" operator action. The goal is to reuse the best compatible local state or rebuild a new base automatically.

## Managed Local Postgres Pattern

For application targets, use host-visible readiness, not only in-container readiness. The app connects through the mapped host port, so a managed DB task should ensure the container, wait until Postgres is ready inside Docker and the host port accepts connections, then run migrations.

Recommended shape:

```go
mgr := database.New()

db := mgr.Desired(inst.ID, database.Config{
    HostPort:     inst.Ports["postgres"],
    Database:     "app_" + inst.ID,
    User:         "devflow",
    Password:     "devflow",
    SnapshotRoot: filepath.Join(worktree, ".devflow", "db-snapshots"),
})

if err := mgr.EnsureRuntime(ctx, db); err != nil {
    return err
}
if err := mgr.WaitReady(ctx, db, 30*time.Second); err != nil {
    return err
}
```

`EnsureRuntime` preserves the data volume, but recreates a stale container when its published host port does not match the current Devflow instance. Avoid unconditional container removal in normal startup paths; it can race Docker and should not be necessary for a preserved-volume workflow.

For a Prisma project, the high-level path is:

```go
result, err := mgr.EnsurePrismaDevDatabase(ctx, database.PrismaDevDatabaseOptions{
    Worktree:      worktree,
    DB:            db,
    SchemaPath:    "prisma/schema.prisma",
    MigrationsDir: "prisma/migrations",
    SourcePolicy:  policy,
    Prepare: database.PrepareOptions{
        Worktree: worktree,
        Env:      env,
    },
})
```

This will:
- inspect the Prisma schema and migration folder
- ignore non-directory entries in `prisma/migrations`, including Prisma's `migration_lock.toml`
- restore the nearest compatible cached migration point
- clone/rebuild a base database when no compatible snapshot exists
- ensure the host-visible Postgres runtime is ready
- run Prisma migrations through `npx prisma migrate deploy`
- snapshot each migration prefix by default

That prefix snapshotting matters during development. If you edit the latest migration, Devflow can restore the previous compatible prefix and apply only the changed tail instead of rebuilding from the remote/base source.

When a detached run is active, `devflow tui` has a `d` database/Prisma panel that shows the managed Postgres identity, recent cached Prisma migration-prefix snapshots, and schema/migration drift. Pressing `m` is an explicit migration-authoring action; normal `up` startup should still avoid hidden migration generation.

Only override `Migrate` when you intentionally want an all-at-once custom Prisma command; that path snapshots the final state only. Use `MigrateEach` for custom per-prefix behavior.

If `schema.prisma` declares models but no migrations exist, or if `schema.prisma` changes but no new migration appears, `EnsurePrismaDevDatabase` returns an explicit error instead of pretending the database is current.

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

When a Prisma schema has changed and a new migration must be authored explicitly, add a separate target/action that uses:

```go
err := database.GeneratePrismaMigration(ctx, database.PrismaMigrationGenerateOptions{
    Worktree:   worktree,
    SchemaPath: "prisma/schema.prisma",
    Name:       "add-bike-ride-index",
    CreateOnly: true,
})
```

Keep migration generation as an explicit target/action, not part of normal `up` startup.

To let the TUI create migrations without guessing paths, implement `project.PrismaConfigProvider`:

```go
func (myProject) PrismaConfig() project.PrismaConfig {
    return project.PrismaConfig{
        SchemaPath:    "prisma/schema.prisma",
        MigrationsDir: "prisma/migrations",
        BasePaths:     []string{"prisma/bootstrap.sql"},
        CreateOnly:    true,
    }
}
```

Then `devflow tui` can flag schema/migration drift in the `d` database/Prisma panel. Press `m`, enter a migration name, and Devflow runs `GeneratePrismaMigration` from inside the TUI. If no provider is implemented, the TUI tries common layouts such as `prisma/schema.prisma` and `db/schema.prisma`.

Typical graph shape:
- `postgres`: service task that calls `EnsureRuntime`, `WaitReady`, and supervises database lifetime/logs
- `db_prepare`: finite task that restores/rebuilds the volume from snapshots or a base source
- `db_migrate`: finite task that runs migrations against the host DSN after host readiness passes
- `app`: service task depending on `db_migrate`

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

For repo-local helper executables, use the built-in binary-tool helper in `pkg/project`.

Example:

```go
tool := project.BinaryTool{
    TaskName: "build_auth_mapping",
    Inputs: project.Inputs{
        Files: []string{"tools/auth-mapping/main.go", "go.mod", "go.sum"},
    },
    Output: ".devflow/tools/auth-mapping",
    Build: process.CommandSpec{
        Name: "go",
        Args: []string{"build", "-o", ".devflow/tools/auth-mapping", "./tools/auth-mapping"},
    },
}
buildTask := tool.BuildTask()

tasks := []project.Task{
    buildTask,
    {
        Name: "auth_mapping",
        Kind: project.KindOnce,
        Deps: []string{buildTask.Name},
        Run: func(ctx context.Context, rt *project.Runtime) error {
            return tool.Run(ctx, rt, "--config", rt.Abs("backend/auth/config.json"))
        },
    },
}
```

Rules:
- the tool output path should be worktree-relative so it can be cached and restored
- keep the binary output outside the input directories you fingerprint, or ignore it explicitly
- use the task `Inputs` to describe what should invalidate the build
- use `Signature` if the build command itself needs a stable explicit version marker

For generated files, prefer an explicit ignore pattern on the task that owns the source directory. Ignore patterns are slash-normalized and are checked both root-relative and, for directory inputs, relative to that input directory. For example, if a task has `Inputs.Dirs: []string{"internal/storage"}`, both `internal/storage/sqlc` and `sqlc` suppress generated files below `internal/storage/sqlc`.

Use this command when tuning watch inputs:

```bash
devflow graph affected --files internal/storage/sqlc/users.sql.go --explain --json
```

This gives you a standard way to compile helper binaries once, cache them by input hash, and run the restored artifact later from downstream tasks.

## Service Readiness

Service tasks can define an optional readiness function.

Current shape:

```go
type ReadyFunc func(ctx context.Context, rt *Runtime) error

type Task struct {
    Name         string
    Kind         Kind
    Run          RunFunc
    Ready        ReadyFunc
    ReadyTimeout time.Duration
    // ...
}
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
