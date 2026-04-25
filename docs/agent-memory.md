# Agent Memory

This is the shared long-term memory for AI work on Devflow.

Use it on every substantial agent session to preserve project health across model changes, context resets, and separate coding runs. It is the common project brain for stable operating principles, current shape, recurring constraints, and active risks that should not live only in chat.

## Read Order

1. `AGENTS.md` for project rules and non-goals.
2. `docs/agent-memory.md` for shared project memory and long-term AI operating context.
3. `PROGRESS.md` for the current implementation ledger, active milestone, next steps, and known gaps.
4. `docs/architecture.md` for package boundaries and runtime state layout.
5. `docs/cli.md` for user-facing command behavior and JSON surfaces.
6. `docs/testing.md` before changing behavior with broad blast radius.

## Memory Policy

- Read this file before substantial work, not only when inheriting from another agent.
- Update this file when an implementation decision changes how future agents should think.
- Keep transient task history in `PROGRESS.md`; keep durable mental models and constraints here.
- Keep subsystem facts in the relevant subsystem docs; use this file to connect the cross-cutting implications future agents need to remember.
- Do not duplicate every changelog entry. Store only context that protects future correctness, maintainability, or product direction.
- If chat contains important project context, move the durable part into docs before ending the work.

## Subsystem Documentation Links

Use this memory together with the subsystem docs. When a change affects one of these areas, update the corresponding doc rather than only editing this file.

- `docs/architecture.md`: package boundaries, runtime state layout, bootstrap flow, cache/env/database design
- `docs/cli.md`: command behavior, JSON output contracts, TUI/operator semantics
- `docs/adapter-guide.md`: adapter authoring expectations and project-local behavior
- `docs/agent-integration.md`: agent-facing execution surfaces and future wrapper direction
- `docs/testing.md`: default and opt-in verification expectations
- `docs/roadmap.md`: active priorities and deferred work

## Working Mindset

- Keep the core generic. Project-specific behavior belongs in adapters, examples, or project-local `devflow.project.go` files.
- Preserve stable JSON output for every user-facing command.
- Treat worktrees as the isolation boundary.
- Keep instance env explicit, layered, and persisted.
- Services are supervised, not cached.
- Cacheable tasks must declare outputs.
- Prefer narrow, semantic fingerprints over hashing the whole repo.
- Optimize cache storage only after correctness and contract coverage exist.

## Current Shape

Devflow is now beyond the initial bootstrap. The core includes graph validation, fingerprinting, snapshot caching, process supervision, instance and port state, bounded parallel engine scheduling, typed events, polling watch mode, dependency checks/installers, interactive prompt plumbing, a TUI, and Docker-backed Postgres runtime helpers.

Runtime adapters are project-local:
- the repo-level `devflow` launcher builds the bootstrap binary
- a selected worktree must contain `devflow.project.go`
- the bootstrap CLI compiles `<worktree>/.devflow/bin/devflow-local`
- normal commands exec into that worktree-local binary
- there is no built-in adapter fallback when `devflow.project.go` is missing

State is split deliberately:
- per-worktree logs and instance snapshots live under the worktree `.devflow/`
- sibling git worktrees share cache and port allocation through the Git common dir
- non-git temp/test flows fall back to local/global safe defaults

## Before Editing

- Check `git status --short` and preserve any user changes.
- Update `PROGRESS.md` at the start and end of substantial work.
- Prefer existing package boundaries and helper APIs over new abstractions.
- Keep public behavior documented when changing CLI output, adapter contracts, or runtime state.
- Avoid hidden interactive subprocesses in normal boot/watch paths; model destructive or ambiguous choices as explicit actions.

## Verification Baseline

Default verification:

```bash
go test ./...
```

Useful command smoke checks:

```bash
go run ./cmd/devflow doctor --json
go run ./cmd/devflow status --json
go run ./cmd/devflow run <target> --json --project <name>
```

Docker-backed database integration tests are opt-in and should be run when changing `pkg/database` runtime behavior:

```bash
DEVFLOW_E2E_DOCKER=1 go test ./pkg/database -run Docker -v
```

## Current Priorities

The latest concrete next steps are maintained in `PROGRESS.md`. As of this memory update, likely next work includes:

- richer watch restart policies
- fine-grained detached service restart/control beyond whole-target relaunch
- project-local adapter loading beyond a single self-contained `devflow.project.go`
- broader TUI operator actions with confirmations and rerun/stop/restart controls
- stronger JSON contract tests for status, instances, and events

## Deliberate Deferrals

- Do not introduce a YAML-first config DSL in v1.
- Do not hardcode Prisma, sqlc, Next.js, or repo-specific paths into core packages.
- Do not replace adapter-owned database base-source policy with implicit reset behavior.
- Do not make real Docker e2e tests part of default `go test ./...` until the project intentionally changes that contract.
