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

## State Layout

Per-worktree state lives under `.devflow/`:
- `.devflow/cache`
- `.devflow/logs/<instance-id>/`
- `.devflow/state/instances/<instance-id>/`

Shared coordination state lives under the user cache directory:
- `devflow/state/ports.json`
- `devflow/state/instance-index.json`

This split keeps cache and logs local to the worktree while still allowing cross-worktree port coordination.

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

Those decisions belong in adapter policy layered on top of the runtime module. The package now provides the snapshot-planning primitives; the adapter still needs to decide when to clone remote state, when to snapshot, and when to fall back to a fresh bootstrap.

The bundled example adapter now exercises this shape structurally:
- inspect Prisma state
- restore the nearest snapshot or reset the volume
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

Planned task-model shape:

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

This is exposed through engine subscription rather than a dedicated CLI command for now. The goal is to keep the event envelope stable before adding TUI and MCP-facing stream surfaces.

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

What is still missing is fine-grained detached control of a single service inside a multi-service detached target.
