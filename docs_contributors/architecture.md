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
- `pkg/daemon`: per-worktree daemon, JSON-line socket protocol, action queue, and event fanout for mutable dev/watch/operator work
- `pkg/event`: typed event bus used by the engine for run, task-state, cache, process, instance, and log events
- `pkg/watch`: Devflow-owned polling file scanner and debounced change batching

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

Daemon/supervisor state is also per worktree. The instance snapshot records:
- the per-worktree daemon PID as the supervisor PID
- legacy child executor PIDs when old supervisor state is being reconciled
- service task PIDs
- the daemon log path

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

The daemon Unix socket lives in a short per-user temp directory such as `/tmp/devflow-daemon-<uid>/<instance-id>.sock`. It is intentionally not stored under deeply nested worktree paths because Unix socket path length limits are easy to hit on macOS.

This split keeps runtime logs and instance state local to the worktree, keeps task cache globally reusable, keeps port allocation coordinated for sibling git worktrees, and keeps socket paths short enough for real terminals and test worktrees.

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
- `Inputs.Paths` matches exact relative paths and descendants
- `Inputs.Files` matches exact relative files
- `Inputs.Dirs` matches relative directories and descendants
- `Inputs.Globs` matches slash-normalized glob patterns, including `**`
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

New user-facing adapters should normally use the builder API, where `Inputs("path")` populates `Inputs.Paths` and `project.Glob("internal/storage/**/*.sql")` populates `Inputs.Globs`. The older `Files`/`Dirs` fields remain the lower-level internal representation for existing engine tests and helpers.

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

`RequiredCLIs()` is the project-level catalog at the engine boundary. New adapters normally populate it through `project.Builder.RequiredCLIs` or `RequiredCLI`. Builder command tasks automatically select a matching catalog entry when the command name matches it. Tasks and targets select from that catalog with `RequiredCLIs`, allowing target-scoped commands to avoid over-reporting tools that belong only to unrelated flows. The older `Dependencies()` provider remains as a compatibility shim for early adapters.

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

Docker control-plane commands such as inspect, start, stop, remove, and detached container start are bounded by short command timeouts. Long data-plane sidecar work such as snapshot archive/restore has a separate longer timeout. Docker commands run in their own process group on Unix-like platforms; timeout/cancel kills the group and uses a bounded wait so Docker Desktop helper children cannot keep stdout/stderr pipes open forever. A stuck Docker Desktop or Docker CLI should surface as a database task error rather than leaving a task in `running` forever.

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
- `database.Postgres` for the common managed local Postgres instance wiring, including default snapshot root, runtime env, and port allocation
- `database.Prisma` for common Prisma tasks: client generation, migration-prefix DB preparation, and explicit migration authoring
- `EnsureMigratedDatabase` for generic migration folders
- `PostgresMigrationFileApplier` for applying one SQL file per migration and snapshotting every prefix
- `EnsurePrismaDevDatabase` for Prisma schema + migration folders, applying pending migrations through prefix-limited `prisma migrate deploy` runs by default
- `PreparePrismaMigrationAuthoringDatabase` for reconciling a managed Prisma database before migration authoring
- `GeneratePrismaMigration` for explicit Prisma migration authoring
- `PostgresDumpSourcePolicy` for cloning a remote development Postgres database into the local runtime

Prisma migration inspection is directory-only. Files under `prisma/migrations`, including `migration_lock.toml`, are not migration points and must not affect prefix counts or snapshot keys.

The important cache invariant is prefix safety. A snapshot can only be reused when its migration list is a valid prefix of the current migration list and the base fingerprint still matches. `EnsureMigratedDatabase` with `ApplyEach` snapshots every prefix after applying it. The default `EnsurePrismaDevDatabase` path is less chatty: committed migration history is treated as stable and applies as one tail before the final snapshot. Intermediate Prisma snapshots are created only at boundaries needed for migration folders with uncommitted Git changes, plus the final state. If Git is unavailable or the worktree is not a Git repository, the default falls back to final-only snapshotting. That preserves local migration editing without running Prisma once per historical committed migration on cold rebuilds. Adapters that need exhaustive Prisma prefix snapshots can still provide `MigrateEach`.

Prisma has two authoring guards: if `schema.prisma` declares models but no migrations exist, or if `schema.prisma` changes but the migration list has not advanced beyond the restored prefix, the default workflow returns a migration-needed error telling the adapter to generate a migration first. The engine writes errors that implement `MigrationNeeded() bool`, plus known Prisma migration-needed messages, as `migration_needed` rather than `failed`, and downstream work remains pending. Migration generation must be modeled as an explicit target/action using `PreparePrismaMigrationAuthoringDatabase` plus `GeneratePrismaMigration`, or as the explicit TUI `m` action, not hidden inside normal `up`.

Prisma database preparation emits progress lines before snapshot planning, runtime recreation, readiness waits, source-policy application, and final runtime start. This is intentionally visible in task logs and the TUI footer because Docker reconciliation can happen before any Prisma subprocess writes output. Progress helpers own writing those component progress lines to the task log and event stream; subprocess output is written to the task log by `pkg/process` and forwarded to live consumers through an event-only callback so Prisma CLI output is not duplicated.

Migration authoring prep intentionally differs from normal DB prep: it restores/rebuilds the managed database to the best compatible prefix, reapplies any missing or edited tail migrations, and does not snapshot the schema-drift state it prepares for Prisma. That lets `prisma migrate dev --create-only` compare the current schema against a compatible database without hitting Prisma's "migration was modified after it was applied" reset prompt after a developer edits the latest migration.

Adapters may override Prisma migration execution with `Migrate` or `MigrateEach`. `Migrate` is an all-at-once command and only snapshots the final state; `MigrateEach` preserves the exhaustive per-prefix cache contract.

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

## Local Task Stamps

Stamped tasks are finite `KindOnce` tasks that use the normal task key but do not write a global cache entry. On success, the engine writes a tiny per-worktree stamp under:

```text
<worktree>/.devflow/state/instances/<instance-id>/task-stamps/
```

On a later run in the same worktree, a matching stamp marks the task done without executing it, provided any declared local outputs still exist. This supports install/setup commands such as `npm install`: the task reruns when `package.json` or `package-lock.json` changes, but Devflow does not copy `node_modules` into `<os.UserCacheDir()>/devflow/cache` or consult the global cache to decide whether the install is already valid. If `node_modules` is declared as an output and is deleted in that worktree, the stamp is ignored and the install task runs again.

Stamped tasks are invalidated together with cacheable tasks in daemon/TUI invalidation flows, but they are not cache hits and should not be reported as restored artifacts.

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

The daemon subscribes to engine events, persists them, and fans them out over its JSON-line socket to live consumers such as the TUI. Direct `run --ci` still uses the engine in-process and writes only the command result unless a future CI event surface is added.

For watch cycles specifically:
- `files` now carries the raw changed worktree-relative file paths from the watcher batch
- `affectedTasks` carries the directly affected task names derived from those file changes

Daemon-owned runs also persist the engine event stream to:
- `.devflow/state/instances/<instance-id>/events.jsonl`

The TUI uses the daemon socket subscription as its primary live-update signal and the persisted event stream as a fallback/recovery source.

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
- attached non-CI `run` is a foreground operator command backed by the per-worktree daemon. It starts services, waits for readiness, and then blocks while those services run. A service exit ends the attached run; an external stop may surface as a service-exited error.
- `run --ci` is deliberately direct and finite, not daemon-backed. Services are allowed as readiness probes: Devflow starts them, waits for readiness, stops them, clears persisted service PIDs, and records the service nodes as `stopped` before returning success.
- `run --detach` asks the per-worktree daemon to start the target in the background and returns after launch. It does not prove the target closure is healthy.
- `watch --detach` asks the daemon to start the development loop. It is the expected long-running mode for humans and agents that want automatic reruns after file edits.
- `flush` is the daemon-backed watch readiness gate. It proves the watcher observed the post-edit sync sentinel, waits for the selected target closure to settle, and checks service health.
- `stop --all` asks the daemon to stop active work. It reconciles legacy supervisors, child executors and their process trees, tracked services, stale status PIDs, and the instance-managed database container. Stopping the database container preserves its Docker volume. After sending the response, the daemon shuts itself down so stopped state is not reported as a live daemon.

The current automation recommendation is intentionally explicit: use detached watch plus `flush` for "background environment is ready" workflows. Do not reinterpret attached `run` as a start-and-return command without adding a separate CLI contract.

Finite check/test targets with service dependencies should generally use `run --ci`, because plain attached `run` keeps service dependencies alive.

## Watch Mode

Watch mode uses a polling watcher with debounced batches. The engine scopes the watcher to the selected target closure's declared file inputs plus the flush sync directory. It does not intentionally poll the whole worktree when the closure has concrete `Inputs(...)`, `Files`, `Dirs`, or `Globs`; common heavyweight folders such as `node_modules` are ignored by default unless explicitly watched by an input path.

On each batch:
- changed files are mapped to task inputs
- the affected downstream slice inside the target closure is computed
- impacted running services are stopped first
- affected one-shot tasks rerun in dependency order with normal cache semantics
- impacted services restart after their dependencies complete and are only considered back once readiness passes

Watch propagation now treats service-to-service dependency edges specially:
- service restarts do not automatically cascade into downstream services by default
- a downstream service must opt into that behavior with `WatchRestartOnServiceDeps`

This prevents backend-service bounces from needlessly forcing frontend-service restarts, while still allowing explicit infrastructure-style dependencies such as `postgres -> backend`.

The watch engine still runs in-process relative to its owner, but for user-facing dev/watch/operator workflows that owner is the per-worktree daemon. This keeps one mutable source of truth for service lifecycle, status, file watching, and event streaming.

## Operator Controls

The current operator surface now includes:
- PID-based `stop` for tracked service tasks
- daemon-backed background launch for service-bearing runs
- cache inspection and invalidation
- cache garbage collection
- limited non-service `restart`
- detached service `restart` by restarting the last daemon target

Mutable ownership is implemented by one daemon per worktree. CLI and TUI operations connect to that daemon over a JSON-line Unix socket. The daemon owns:
- active engine run/watch context
- service process handles
- status writes
- event persistence and live event fanout
- flush sync/ack coordination
- TUI actions such as invalidate/rerun, retarget, and Prisma migration authoring

Daemon startup is serialized by a per-instance file lock under worktree state so concurrent CLI/TUI commands cannot start competing daemons for the same worktree.

TUI daemon ownership is explicit. If bare `devflow` or `devflow tui` creates the daemon for that TUI session, TUI exit sends the normal all-work stop request so active services, managed databases, and the daemon shut down together. If the TUI connects to a daemon that already existed, TUI exit only disconnects the UI.

The older hidden `__internal_exec` and `__internal_supervise` launcher paths are no longer user-facing execution routes. Their persisted supervisor/executor state and process-tree descendants can still be reconciled during `stop --all` so existing stale processes are not orphaned.

The daemon persists:
- daemon PID as the supervisor PID
- daemon log path
- last run config
- legacy supervisor/executor PIDs as process refs when replacing old state

This is enough for:
- `run --detach`
- `watch --detach`
- `stop --all` against daemon-owned work; it terminates legacy supervisor/executor process groups and descendants, tracked service process groups, PID-bearing status nodes, and the instance-managed database container before clearing persisted process state and shutting the daemon down
- service `restart` by asking the daemon to relaunch the last active target

The operator surface now also reconciles detached state when queried:
- `status` uses the daemon when one is already running, otherwise reads persisted state without starting a new daemon; it includes daemon PID/liveness plus sanitized instance metadata such as ports, URLs, and DB identity when present
- `logs supervisor` reads the daemon/supervisor log directly

The first usable TUI slice is now implemented as a local terminal console connected to the per-worktree daemon, with persisted state as fallback. It currently provides:
- live daemon event subscription plus fallback persisted-event refresh
- task selection
- selected-task details
- task log tail
- daemon/supervisor log toggle
- database/Prisma panel showing managed Postgres identity and recent Prisma migration-prefix snapshots
- explicit Prisma migration generation from inside the TUI by asking for a migration name, sending a daemon action, running the project migration target through the daemon-owned engine, and relaunching the previously detached target after success
- instance/worktree/runtime header
- stable terminal rendering via a real TUI library instead of manual ANSI frame painting
- invalidate-and-rerun from the selected task by sending a daemon action that invalidates the selected downstream cacheable once-task slice and relaunches the current target
- prompt popups for interactive confirm and text questions emitted by daemon-owned work

What is still missing is fine-grained detached control of a single service inside a multi-service detached target.
