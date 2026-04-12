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
- `pkg/event`: event types and an in-memory bus used by the engine

## State Layout

Per-worktree state lives under `.devflow/`:
- `.devflow/cache`
- `.devflow/logs/<instance-id>/`
- `.devflow/state/instances/<instance-id>/`

Shared coordination state lives under the user cache directory:
- `devflow/state/ports.json`
- `devflow/state/instance-index.json`

This split keeps cache and logs local to the worktree while still allowing cross-worktree port coordination.
