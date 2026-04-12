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
- `pkg/engine`: sequential execution engine and status persistence
- `pkg/engine`: bounded parallel ready-queue execution engine and status persistence
- `pkg/event`: typed event bus used by the engine for run, task-state, cache, process, instance, and log events

## State Layout

Per-worktree state lives under `.devflow/`:
- `.devflow/cache`
- `.devflow/logs/<instance-id>/`
- `.devflow/state/instances/<instance-id>/`

Shared coordination state lives under the user cache directory:
- `devflow/state/ports.json`
- `devflow/state/instance-index.json`

This split keeps cache and logs local to the worktree while still allowing cross-worktree port coordination.

## Event Stream

The engine now emits a typed in-process event stream for live consumers. Event categories include:
- run started / finished
- instance updated
- task state changed
- cache hit / miss
- log line
- process exited

This is exposed through engine subscription rather than a dedicated CLI command for now. The goal is to keep the event envelope stable before adding TUI and MCP-facing stream surfaces.
