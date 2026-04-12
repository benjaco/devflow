# Architecture

## Core Layers

`cmd/devflow` is thin CLI wiring over the packages in `pkg/`.

- `pkg/project`: task, target, runtime, and adapter interfaces
- `pkg/graph`: validation, topo ordering, closures, and affected-task calculation
- `pkg/fingerprint`: deterministic file, directory, env, and task-key hashing
- `pkg/cache`: manifest, snapshot, restore, and cache lookup
- `pkg/process`: one-shot execution, supervised services, line-buffered logs
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

## Watch Mode

Watch mode now uses a polling watcher with debounced batches. On each batch:
- changed files are mapped to task inputs
- the affected downstream slice inside the target closure is computed
- impacted running services are stopped first
- affected one-shot tasks rerun in dependency order with normal cache semantics
- impacted services restart after their dependencies complete

The current implementation is local and in-process. It intentionally prioritizes correctness and selective reruns over elaborate optimization.
