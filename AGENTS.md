# AGENTS.md

## Project Purpose

Devflow is a local-first DAG runner for development workflows. Keep the core generic and push project-specific behavior into adapters and examples.

## Non-Goals

- Do not hardcode Prisma, sqlc, Next.js, or repo-specific paths into core packages.
- Do not introduce a YAML-first config DSL in v1.
- Do not optimize cache storage before correctness exists.

## Key Commands

- `go test ./...`
- `go run ./cmd/devflow doctor --json`
- `go run ./cmd/devflow run <target> --json --project <name>`
- `go run ./cmd/devflow status --json`

## Context Bootstrap

- Start with `docs_contributors/agent-memory.md` and `PROGRESS.md` before substantial work.
- Use `docs_contributors/architecture.md`, `docs_contributors/cli.md`, and `docs_contributors/testing.md` for package boundaries, command contracts, and validation expectations.
- Treat `docs_contributors/agent-memory.md` as shared long-term project memory for AI agents; keep durable project context there instead of only in chat.

## Subsystem Documentation

- Use `docs_contributors/agent-memory.md` as the entry point for durable AI context, but do not let it become detached from the actual software design.
- When work changes a subsystem's behavior, contracts, boundaries, risks, or operator model, update the relevant subsystem documentation in the same change:
  - architecture/package boundaries in `docs_contributors/architecture.md`
  - CLI behavior and JSON surfaces in `docs_contributors/cli.md`
  - adapter expectations in `docs_users/adapter-guide.md`
  - agent-facing execution surfaces in `docs_users/agent-integration.md`
  - verification expectations in `docs_contributors/testing.md`
  - roadmap priorities in `docs_contributors/roadmap.md`
- Update `docs_contributors/agent-memory.md` when the change creates durable context that future agents should carry across subsystems, such as a design invariant, repeated failure mode, long-term deferral, or project-specific mental model.
- Prefer documenting subsystem facts in their subsystem docs and linking or summarizing the cross-cutting lesson in `docs_contributors/agent-memory.md`.

## Engineering Rules

- Every user-facing command must support stable JSON output, except `devflow docs`, which intentionally prints plain bundled user Markdown only.
- Service tasks are supervised, not cached.
- Cached tasks must declare outputs.
- Worktree is the isolation boundary.
- Instance env must be explicit and persisted.
- Prefer narrow fingerprints over hashing the entire repo.

## Progress Tracking

- Maintain the file [PROGRESS.md](/Users/benjaminschultzlarsen/Desktop/devflow/PROGRESS.md) as the canonical implementation ledger.
- Update `PROGRESS.md` at the start and end of substantial work so repo state is recoverable without chat history.
- Update `docs_contributors/agent-memory.md` when durable project context, mental models, or recurring constraints change.
- Record:
  - current milestone/status
  - completed work
  - in-progress work
  - next concrete steps
  - known gaps or deliberate deferrals
- Keep entries concise and factual. Do not turn `PROGRESS.md` into a narrative changelog.

## Done Means

- tests pass
- JSON contracts remain stable
- docs updated for public API changes
- examples still build
