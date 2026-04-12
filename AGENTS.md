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

## Engineering Rules

- Every user-facing command must support stable JSON output.
- Service tasks are supervised, not cached.
- Cached tasks must declare outputs.
- Worktree is the isolation boundary.
- Instance env must be explicit and persisted.
- Prefer narrow fingerprints over hashing the entire repo.

## Progress Tracking

- Maintain the file [PROGRESS.md](/Users/benjaminschultzlarsen/Desktop/devflow/PROGRESS.md) as the canonical implementation ledger.
- Update `PROGRESS.md` at the start and end of substantial work so repo state is recoverable without chat history.
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
