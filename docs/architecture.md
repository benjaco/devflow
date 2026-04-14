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
- the repo-level `devflow` launcher builds the bootstrap CLI in the `devflow` repo
- when invoked in a project worktree, the bootstrap CLI looks for `./devflow.project.go`
- if the file is missing, the command fails
- if the file exists, the bootstrap CLI compiles a worktree-local full CLI binary
- execution is then transferred into that compiled local binary for all normal commands

Current local binary location:
- `<worktree>/.devflow/bin/devflow-local`

Current generated build location:
- `<devflow-repo>/.devflow/localbuild/<worktree-hash>/`

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
- `.devflow/cache`
- `.devflow/logs/<instance-id>/`
- `.devflow/state/instances/<instance-id>/`

Shared coordination state lives under the user cache directory:
- `devflow/state/ports.json`
- `devflow/state/instance-index.json`

This split keeps cache and logs local to the worktree while still allowing cross-worktree port coordination.

## Runtime Env

Instance env is now explicit and layered:
- optional `.env` file values loaded by the adapter
- adapter-defined static env
- devflow-managed instance overrides such as ports and database connection values

The important rule is precedence:
- dotenv values are the base
- devflow-managed runtime values win

That allows projects to keep normal local app settings in `.env` while still ensuring the launched frontend/backend processes point at the correct per-instance Postgres runtime and leased ports.

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

## Dependency Installation

Adapters can now define project-scoped command dependencies together with platform-specific install scripts.

Current shape:

```go
type Dependency struct {
    Name        string
    Command     string
    Description string
    Install     map[string]InstallScript
}
```

Semantics:
- dependency status is determined by checking whether the command is available on `PATH`
- `deps install` only runs installers for commands that are currently missing
- after an installer runs, Devflow re-checks that the command now resolves
- install scripts are selected by platform (`darwin`, `linux`, `windows`, or `unix`)

This keeps dependency policy adapter-defined while giving the core CLI a stable install surface for humans, CI, and future agents.

## Database Isolation

The chosen direction is now full per-worktree separation for local databases:
- one Postgres container per worktree instance
- one dedicated host port per worktree instance
- one dedicated Docker volume per worktree instance

The new `pkg/database` package provides the runtime primitives for that model:
- derive deterministic per-instance container and volume names
- ensure the container is running
- wait for readiness via `pg_isready`
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
- supervisor log path
- last detached run config

This is enough for:
- `run --detach`
- `watch --detach`
- `stop --all` against detached runs
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
- instance/worktree/runtime header
- stable terminal rendering via a real TUI library instead of manual ANSI frame painting
- invalidate-and-rerun from the selected task by invalidating the selected downstream cacheable once-task slice and relaunching the current target
- prompt popups for interactive confirm and text questions emitted by the running supervisor

What is still missing is fine-grained detached control of a single service inside a multi-service detached target.
