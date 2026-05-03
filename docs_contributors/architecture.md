# Architecture

## Core Layers

`cmd/devflow` is thin CLI wiring over the packages in `pkg/`.

- `pkg/project`: task, target, runtime, and adapter interfaces
- `pkg/graph`: validation, topo ordering, closures, and affected-task calculation
- `pkg/fingerprint`: deterministic file, directory, env, and task-key hashing
- `pkg/cache`: manifest, snapshot, restore, and cache lookup
- `pkg/process`: one-shot execution, supervised services, line-buffered logs
- `pkg/project`: also defines readiness hooks for service tasks
- `pkg/database`: Docker-backed dedicated Postgres runtime and snapshot helpers
- `pkg/instance`: worktree-scoped instance identity and persisted state
- `pkg/ports`: shared port registry with lock-safe allocation
- `pkg/engine`: bounded parallel ready-queue execution engine and status persistence
- `pkg/event`: typed event bus used by the engine for run, task-state, cache, process, instance, and log events
- `pkg/watch`: polling-based file watching and debounced change batching built on `github.com/radovskyb/watcher`

## Local Project Bootstrap

Runtime project configuration is now project-local.

Flow:
- the installed `devflow` binary, or the repo-level `devflow` launcher during source development, looks for `./devflow.project.go` in the selected project worktree
- if the file is missing, the command fails
- if the file exists, the bootstrap CLI compiles a worktree-local full CLI binary
- stale checks and rebuilds are guarded by a per-worktree lock at `<worktree>/.devflow/localbuild.lock`; commands that waited for another builder re-check the build key before compiling
- execution is then transferred into that compiled local binary for all normal commands

Current local binary location:
- `<worktree>/.devflow/bin/devflow-local`

Current generated build location:
- `<worktree>/.devflow/localbuild/<worktree-hash>/`

Current localbuild lock location:
- `<worktree>/.devflow/localbuild.lock`

The generated build is a small Go module whose path is under `github.com/benjaco/devflow/localbuild/...`, which allows it to import Devflow's `internal/cli` package. Installed binaries require the released Devflow module version. Source-local development uses:

```go
replace github.com/benjaco/devflow => <devflow-source-root>
```

in that generated module, preserving fast local iteration without requiring a released tag for every source change.

Current first-version constraint:
- `devflow.project.go` is compiled as a self-contained `package main` file
- the project should register itself in `init()`
- this version does not yet load arbitrary companion adapter Go files from the project repo

This model intentionally avoids:
- built-in runtime adapter registries
- runtime JSON adapter protocols
- dynamic plugin loading tricks

## State Layout

Per-worktree state lives under `.devflow/`:
- `.devflow/logs/<instance-id>/`
- `.devflow/state/instances/<instance-id>/`
- `.devflow/state/instances/<instance-id>/flush/`

Detached supervisor state is also per worktree. The instance snapshot records:
- the supervisor PID
- the child `__internal_exec` PID when the supervisor has started it
- service task PIDs
- the supervisor log path, which also contains a `child pid=<pid>` line as a fallback for older state

Task cache storage is global for the user:
- `<os.UserCacheDir()>/devflow/cache`

Entries are namespaced inside that physical cache root:
- `entries/<project-cache-namespace>/<task>/<fingerprint-key>/`

Projects can implement `CacheNamespace() string`; otherwise the project name is used. This keeps one cache folder on the system while avoiding accidental collisions between project adapters.

Repo-shared coordination state for sibling git worktrees still lives under the Git common dir from:
- `git rev-parse --git-common-dir`

Current shared paths:
- `<git-common-dir>/devflow/state/ports.json`

Global coordination state that is not repo-specific still lives under the user cache directory:
- `devflow/state/instance-index.json`

This split keeps runtime logs and instance state local to the worktree, keeps task cache globally reusable, and keeps port allocation coordinated for sibling git worktrees.

Flush coordination is per instance:
- `flush/requests/<request-id>.json` records the requested sync point
- `flush/sync/<request-id>.sync` is the file-watcher sentinel
- `flush/acks/<request-id>.json` stores the final `FlushResult`
- `flush/watch.ready` is an internal readiness marker written after detached watch startup has reached the file-watching phase

## Runtime Env

Instance env is now explicit and layered:
- optional `.env` file values loaded by the adapter
- adapter-defined static env
- devflow-managed instance overrides such as ports and database connection values

The important rule is precedence:
- dotenv values are the base
- devflow-managed runtime values win

That allows projects to keep normal local app settings in `.env` while still ensuring the launched frontend/backend processes point at the correct per-instance Postgres runtime and leased ports.

Instance env is persisted under `.devflow/state` so detached supervisors, status, and relaunches can recover the same runtime configuration. Do not treat it as encrypted secret storage. Adapters should avoid storing long-lived production secrets there, avoid logging full env maps, and override runtime values such as `PORT` for unit-test tasks when those tests should not inherit the service runtime port.

## Watch Cascades

Watch mode uses task inputs as the file-change interface:
- `Inputs.Files` matches exact relative files
- `Inputs.Dirs` matches relative directories and descendants
- `Inputs.Ignore` can suppress matching paths

When a file batch arrives, the engine:
- finds directly affected tasks in the selected target closure
- expands through the downstream task graph using watch restart policy rules
- prunes tasks that cannot run in watch mode, such as warmups without `AllowInWatch` and services with `RestartNever`
- adds services marked `RestartAlways` so they restart on any watch cycle that affects the selected target
- preserves dependency barriers while pruning

The dependency-barrier rule is important: if an intermediate candidate is blocked from the watch cycle, its downstream candidates are blocked too. Downstream tasks must not run in advance against stale intermediate outputs just because they are also reachable from the changed task.

Normal ready-queue scheduling still applies to the final rerun set, so included downstream tasks become runnable only after included upstream dependencies finish or restore from cache.

Ignore semantics are shared by watch matching and fingerprinting:
- paths are slash-normalized before matching
- glob-style matches use exact path matching
- a non-glob directory pattern also suppresses descendants by path prefix
- for directory inputs, ignore patterns are checked both root-relative and relative to the input directory
- for explicit file inputs, root-relative ignore patterns can suppress that file from both watch matching and fingerprinting

This lets adapters use either `internal/storage/sqlc` or `sqlc` to ignore generated files under `Inputs.Dirs: []string{"internal/storage"}`. `devflow graph affected --files <path> --explain --json` exposes which input matched or which ignore pattern suppressed a file.

Current service restart policy meanings in watch mode:
- `RestartNever`: never restart from file-change cascades
- `RestartOnInputChange`: restart only when the service is in the affected downstream slice
- `RestartAlways`: restart on every watch cycle that has at least one directly affected task in the selected target

## Flush Readiness Gate

`devflow flush [target]` coordinates with detached watch mode through the per-instance flush files. The command writes a request file and then writes the sync sentinel under `.devflow/state/instances/<instance-id>/flush/sync/`. While waiting for the ack, the CLI periodically rewrites the sync sentinel. This makes the first flush after `watch --detach` resilient to watcher startup races where the file watcher has written `watch.ready` but has not completed its first polling scan yet.

The watch runner normally ignores `.devflow`, but the engine explicitly includes the flush sync directory in its watcher inputs. When a batch arrives, the engine splits flush sync files out of normal changed files:
- normal user file changes run through the existing watch cascade logic first
- sync files are not treated as task inputs
- after the cycle completes, the engine loads each matching request and writes an ack
- sync-only batches still produce an ack after health evaluation

This proves that edits completed before the `flush` command wrote the sentinel have crossed the watcher's file-change boundary before the command returns success.

Flush health is scoped to the selected target closure:
- once, group, and warmup tasks must be `done` or `cached`
- service tasks must be `running`
- service PIDs must still be alive
- service readiness hooks must pass when defined
- services outside the selected target closure are not part of flush success

Version 1 reports unhealthy in-chain services as `service_unhealthy` issues. It does not auto-restart unhealthy services during `flush`.

## Interactive Commands

Devflow should treat subprocess interactivity as an exception, not the default execution model.

Policy:
- normal `run`, `watch`, and boot targets should be non-interactive
- adapters should prefer explicit non-interactive flags such as `-y`, `--yes`, `--force`, or `CI=1` where that is safe
- if a task would require a destructive or ambiguous choice, the adapter should model that as an explicit action or separate target instead of letting the process block on stdin

This keeps DAG execution deterministic and prevents background runs, detached supervisors, and watch mode from hanging on hidden prompts.

### Prisma-Specific Rules

Prisma needs special handling because its CLI mixes normal migration application with authoring and reset flows.

Rules:
- normal startup flows should not depend on interactive `prisma migrate dev`
- normal DB prep should restore a snapshot, then apply the known remaining migrations non-interactively
- creating a new migration should be a separate explicit operator action because it requires a provided migration name
- destructive reset should be a separate explicit operator action and should not happen implicitly during boot

Recommended command usage:
- create a named migration:
  - `prisma migrate dev --name <name>`
- create the migration without applying it yet:
  - `prisma migrate dev --name <name> --create-only`
- reset only when the user has explicitly chosen reset:
  - `prisma migrate reset --force`

Important limitation:
- from Devflow's perspective, `prisma migrate dev` is still not fully deterministic in drift/reset scenarios, because Prisma may still require an operator decision

Design implication:
- migration authoring and reset flows belong in explicit commands, TUI actions, or future interactive task support
- normal boot/watch targets should stay on the snapshot-plus-replay path instead of relying on Prisma prompts

### Implemented Interactive Prompt Path

Devflow now supports prompt-driven interactive one-shot commands without blocking invisibly in detached mode.

Current behavior:
- tasks can mark a subprocess command as interactive through `process.CommandSpec`
- the command declares expected prompt patterns and prompt kinds
- when a prompt pattern is detected in subprocess output, the engine emits an `interaction_requested` event
- the engine waits for an answer file under the instance state directory
- when an answer arrives, the engine writes it back to the subprocess stdin and emits `interaction_answered`

The current transport is file-backed:
- request metadata is carried on the event stream
- answers are written to `.devflow/state/instances/<instance-id>/interactions/<prompt-id>.json`

This is enough for detached runs and the TUI to cooperate without shared in-memory state.

Current limitation:
- this is prompt-pattern and stdin based, not full TTY emulation
- commands that require a true terminal rather than prompt/answer stdin handling still need a future PTY-specific path

## Required CLI Installation

Adapters define required command-line tools together with platform-specific install scripts.

`RequiredCLIs()` is the project-level catalog. Tasks and targets select from that catalog with `RequiredCLIs`, allowing target-scoped commands to avoid over-reporting tools that belong only to unrelated flows. The older `Dependencies()` provider remains as a compatibility shim for early adapters.

Current shape:

```go
type RequiredCLI struct {
    Name        string
    Command     string
    Description string
    Install     map[string]InstallScript
}

type Task struct {
    Deps         []string
    RequiredCLIs []string
}

type Target struct {
    RootTasks    []string
    RequiredCLIs []string
}
```

Semantics:
- required CLI status is determined by checking whether the command is available on `PATH`
- `devflow doctor` checks the full project required CLI catalog for backward compatibility
- `devflow doctor --target <target>` checks only CLIs required by the target and its task closure
- `devflow clis status/install --target <target>` use the same scoped selection
- `RequiredCLIs` entries may reference either required CLI `Name` or `Command`
- `clis install` only runs installers for commands that are currently missing
- after an installer runs, Devflow re-checks that the command now resolves
- install scripts are selected by platform (`darwin`, `linux`, `windows`, or `unix`)

This keeps required CLI policy adapter-defined while giving the core CLI a stable install surface for humans, CI, and agents.

## Database Isolation

The chosen direction is now full per-worktree separation for local databases:
- one Postgres container per worktree instance
- one dedicated host port per worktree instance
- one dedicated Docker volume per worktree instance

The new `pkg/database` package provides the runtime primitives for that model:
- derive deterministic per-instance container and volume names
- ensure the container is running, recreating stale containers whose published host port no longer matches the selected instance
- wait for readiness via `pg_isready` plus host-port readiness when the DB instance has a host
- stop or destroy the runtime
- snapshot and restore the Postgres data volume
- inspect Prisma schema/migration state and choose the nearest cached migration-prefix snapshot

This keeps DB isolation strong and avoids shared-cluster coupling between worktrees.

What this package does not decide:
- when to clone remote state versus run a bootstrap script
- which schema fingerprint should own the snapshot key

Those decisions belong in adapter policy layered on top of the runtime module. The package now provides the snapshot-planning primitives plus a source-policy hook for snapshot misses; the adapter still needs to decide which base source to use, when to snapshot, and which inputs define the base fingerprint.

### DB Source Policies

Snapshot misses should rebuild from a configured base source, not from an implicit reset action.

Current shape:

```go
type SourcePolicy interface {
    Name() string
    PrepareBase(ctx context.Context, db api.DBInstance, opts PrepareOptions) error
}
```

Behavior:
- first try an exact or nearest-prefix snapshot restore
- if that fails, destroy the current runtime/volume
- if a source policy is configured:
  - start a temporary local Postgres runtime
  - apply the source policy
  - stop the runtime
- then continue with normal migration replay and snapshotting

This matches the intended operator model:
- reuse the latest compatible local volume when possible
- otherwise rebuild from a configured base source such as:
  - a remote dev clone script
  - a local bootstrap/startup script later
- never "skip" a changed migration in the middle; restore falls back only by valid prefix

The bundled example adapter now exercises this shape structurally:
- inspect Prisma state
- restore the nearest snapshot or recreate from the configured base source
- start a temporary DB runtime
- replay remaining migrations
- snapshot the prepared state
- start the final per-instance Postgres service for app runtime

Higher-level workflow helpers now exist on top of those primitives:
- `EnsureMigratedDatabase` for generic migration folders
- `PostgresMigrationFileApplier` for applying one SQL file per migration and snapshotting every prefix
- `EnsurePrismaDevDatabase` for Prisma schema + migration folders, applying pending migrations through prefix-limited `prisma migrate deploy` runs by default
- `GeneratePrismaMigration` for explicit Prisma migration authoring
- `PostgresDumpSourcePolicy` for cloning a remote development Postgres database into the local runtime

Prisma migration inspection is directory-only. Files under `prisma/migrations`, including `migration_lock.toml`, are not migration points and must not affect prefix counts or snapshot keys.

The important cache invariant is prefix safety. A snapshot can only be reused when its migration list is a valid prefix of the current migration list and the base fingerprint still matches. `EnsureMigratedDatabase` with `ApplyEach` and the default `EnsurePrismaDevDatabase` path snapshot every prefix after applying it, so editing the latest migration can restore the previous prefix snapshot and apply only the changed tail.

Prisma has two authoring guards: if `schema.prisma` declares models but no migrations exist, or if `schema.prisma` changes but the migration list has not advanced beyond the restored prefix, the default workflow returns an error telling the adapter to generate a migration first. Migration generation must be modeled as an explicit target/action using `GeneratePrismaMigration`, or as the explicit TUI `m` action, not hidden inside normal `up`. The TUI action starts and waits for the recorded managed database when the instance has one, reports progress through the footer status, then runs Prisma migration generation.

Adapters may override Prisma migration execution with `Migrate` or `MigrateEach`. `Migrate` is an all-at-once command and only snapshots the final state; `MigrateEach` preserves the per-prefix cache contract.

`PostgresDumpSourcePolicy` must fail when `pg_dump` fails. It writes through a temporary dump file instead of an unchecked shell pipeline so `psql` cannot mask a failed clone with an empty successful restore.

Managed Postgres target pattern:
- preserve the Docker volume unless an explicit restore/rebuild path owns the destruction
- call `EnsureRuntime` before migration/app tasks
- call `WaitReady` before connecting through the host DSN; it checks Docker readiness and the host-mapped port
- run migrations against `db.URL`, not a container-local address
- stop the final DB container through `devflow stop --all`; this preserves the Docker volume

Do not unconditionally remove the DB container in normal startup. Docker port mappings are immutable, so `EnsureRuntime` removes and recreates only stale containers with a wrong published port while preserving the volume.

## Cache Keys

The default cache key is derived automatically from:
- engine key version
- task name
- normalized task signature
- dependency result keys
- selected file and directory hashes
- selected env values
- custom fingerprint outputs

### Task-Defined Cache Key Override

Some tasks can compute a better semantic cache identity than the generic automatic key. The design therefore allows a cacheable one-shot task to define its own cache-key function.

Current task-model shape:

```go
type CacheKeyFunc func(ctx context.Context, rt *Runtime) (string, error)

type Task struct {
    // ...
    CacheKeyOverride CacheKeyFunc
}
```

Semantics:
- `CacheKeyOverride` is optional.
- It applies only to cacheable `KindOnce` tasks.
- When present, it replaces the automatic key body for that task.
- The engine should still namespace the final key with at least:
  - engine key version
  - task name
  - the override result
- The engine should not silently mix automatic inputs into an override key. If a task chooses override mode, that override is authoritative.

Recommended use cases:
- backend artifact fingerprints
- DB-state fingerprints
- adapter-specific semantic versions
- externally known content digests

Guideline:
- use the automatic key unless the adapter can define a narrower and more correct semantic key
- when using an override, the task author is responsible for including any dependency/version/config data needed for correctness

## Built Binary Helpers

`pkg/project` now includes a generic helper for software-build tools that need to be compiled once and reused by later tasks.

Current shape:

```go
type BinaryTool struct {
    TaskName    string
    Description string
    Deps        []string
    Inputs      Inputs
    Output      string
    Build       process.CommandSpec
    Signature   string
    Tags        []string
}
```

Semantics:
- `BuildTask()` returns a cacheable `KindOnce` task
- the task key still comes from the normal task-input fingerprint model
- the built artifact is cached as a declared output file
- later tasks can call `tool.Run(...)` or `tool.Start(...)` to execute the built artifact

This is intended for helper binaries such as code generators, schema tools, or repo-local build utilities. The helper stays generic: the engine does not know how the binary is produced, only which inputs fingerprint it and which output file should be cached.

## Event Stream

The engine now emits a typed in-process event stream for live consumers. Event categories include:
- run started / finished
- watch cycle started / finished
- instance updated
- task state changed
- cache hit / miss
- log line
- process exited
- interaction requested / answered / cancelled

This is exposed through engine subscription rather than a dedicated CLI command for now. The goal is to keep the event envelope stable before adding TUI and MCP-facing stream surfaces.

For watch cycles specifically:
- `files` now carries the raw changed worktree-relative file paths from the watcher batch
- `affectedTasks` carries the directly affected task names derived from those file changes

Detached runs now also persist the engine event stream to:
- `.devflow/state/instances/<instance-id>/events.jsonl`

The TUI uses that persisted event stream as its primary live-update signal.

## Service Readiness

Service tasks can now declare adapter-defined readiness checks.

Current task-model shape:

```go
type ReadyFunc func(ctx context.Context, rt *Runtime) error

type Task struct {
    // ...
    Ready        ReadyFunc
    ReadyTimeout time.Duration
}
```

Semantics:
- readiness is optional and applies to service tasks
- the process is started first
- the task is only marked `running` after readiness passes
- if readiness fails, times out, or the process exits first, the task becomes `failed`
- a failed readiness attempt stops the service process before returning

The current helper surface includes:
- `ReadyAll(...)`
- `ReadyFile(...)`
- `ReadyPath(...)`
- `ReadyTCPPort(...)`
- `ReadyHTTPNamedPort(...)`

Default behavior:
- tasks without `Ready` are considered ready immediately after process start
- tasks with `Ready` use a default timeout when `ReadyTimeout` is unset

This keeps the core generic while letting adapters define the right readiness signal for each service.

## Service Lifecycle Contract

Service tasks have different command semantics depending on the run mode:
- attached `run` is a foreground operator command. It starts services, waits for readiness, and then blocks while those services run. A service exit ends the attached run; an external stop may surface as a service-exited error.
- `run --ci` is finite. Services are allowed as readiness probes: Devflow starts them, waits for readiness, stops them, clears persisted service PIDs, and records the service nodes as `stopped` before returning success.
- `run --detach` starts the detached supervisor and returns after launch. It records the supervisor but does not prove the target closure is healthy.
- `watch --detach` starts the detached development loop. It is the expected long-running mode for humans and agents that want automatic reruns after file edits.
- `flush` is the detached watch readiness gate. It proves the watcher observed the post-edit sync sentinel, waits for the selected target closure to settle, and checks service health.
- `stop --all` is the cleanup surface for detached runs. It reconciles supervisor, child executor, tracked service, stale status PIDs, and the instance-managed database container before clearing persisted runtime process state. Stopping the database container preserves its Docker volume.

The current automation recommendation is intentionally explicit: use detached watch plus `flush` for "background environment is ready" workflows. Do not reinterpret attached `run` as a start-and-return command without adding a separate CLI contract.

Finite check/test targets with service dependencies should generally use `run --ci`, because plain attached `run` keeps service dependencies alive.

## Watch Mode

Watch mode now uses a polling watcher with debounced batches. On each batch:
- changed files are mapped to task inputs
- the affected downstream slice inside the target closure is computed
- impacted running services are stopped first
- affected one-shot tasks rerun in dependency order with normal cache semantics
- impacted services restart after their dependencies complete and are only considered back once readiness passes

Watch propagation now treats service-to-service dependency edges specially:
- service restarts do not automatically cascade into downstream services by default
- a downstream service must opt into that behavior with `WatchRestartOnServiceDeps`

This prevents backend-service bounces from needlessly forcing frontend-service restarts, while still allowing explicit infrastructure-style dependencies such as `postgres -> backend`.

The current implementation is local and in-process. It intentionally prioritizes correctness and selective reruns over elaborate optimization.

## Operator Controls

The current operator surface now includes:
- PID-based `stop` for tracked service tasks
- detached supervisor launch for service-bearing runs
- cache inspection and invalidation
- cache garbage collection
- limited non-service `restart`
- detached service `restart` by restarting the last detached target

Detached ownership is currently implemented by spawning a background `devflow` supervisor process and persisting:
- supervisor PID
- child `__internal_exec` PID
- supervisor log path
- last detached run config

This is enough for:
- `run --detach`
- `watch --detach`
- `stop --all` against detached runs; it terminates the supervisor process group, child executor process group, tracked service process groups, PID-bearing status nodes, and the instance-managed database container before clearing persisted process state
- service `restart` by stopping the detached supervisor and relaunching the last detached target

The operator surface now also reconciles detached state when queried:
- `status` includes supervisor PID/liveness plus sanitized instance metadata such as ports, URLs, and DB identity
- if the persisted detached supervisor PID is no longer alive, `status` clears the supervisor record and marks nonterminal nodes as `stopped`
- `logs supervisor` reads the persisted supervisor log directly

The first usable TUI slice is now implemented as a local terminal console over persisted instance state. It currently provides:
- live polling refresh
- task selection
- selected-task details
- task log tail
- supervisor log toggle
- database/Prisma panel showing managed Postgres identity and recent Prisma migration-prefix snapshots
- explicit Prisma migration generation from inside the TUI by asking for a migration name and running the configured/detected Prisma generate command
- instance/worktree/runtime header
- stable terminal rendering via a real TUI library instead of manual ANSI frame painting
- invalidate-and-rerun from the selected task by invalidating the selected downstream cacheable once-task slice and relaunching the current target
- prompt popups for interactive confirm and text questions emitted by the running supervisor

What is still missing is fine-grained detached control of a single service inside a multi-service detached target.
