# Progress

Last updated: 2026-04-12

## Current Status

- Phase: post-bootstrap core implementation
- State: graph/cache/process/instance/ports/engine/CLI foundation implemented and tested, with bounded parallel scheduling and a typed event stream
- Confidence: core parallel run path is working; watch mode and TUI are not implemented yet

## Completed

- Repository scaffold created
- Root docs created:
  - `README.md`
  - `AGENTS.md`
  - `docs/overview.md`
  - `docs/architecture.md`
  - `docs/testing.md`
  - `docs/cli.md`
  - `docs/adapter-guide.md`
  - `docs/agent-integration.md`
  - `docs/roadmap.md`
- Core generic packages implemented:
  - `pkg/api`
  - `pkg/project`
  - `pkg/graph`
  - `pkg/fingerprint`
  - `pkg/cache`
  - `pkg/process`
  - `pkg/instance`
  - `pkg/ports`
  - `pkg/event`
  - `pkg/engine`
- Bounded parallel ready-queue scheduling implemented in `pkg/engine`
- Typed engine event stream implemented for run/task/cache/process/log events
- CLI commands implemented:
  - `run`
  - `status`
  - `logs`
  - `instances`
  - `doctor`
  - `graph list`
  - `graph show`
  - `graph affected`
- `run --max-parallel` wired through the CLI and engine request model
- Example adapter added at `examples/go-next-monorepo`
- Unit/integration-style tests added for core packages
- Verified with `go test ./...`

## In Progress

- No active implementation in progress

## Next Steps

- Implement real watch-mode invalidation and selective reruns in `pkg/watch`
- Expand CLI coverage for restart/stop/cache subcommands
- Build the first usable TUI slice in `pkg/tui`
- Replace the placeholder example adapter behavior with a more realistic adapter contract exercise

## Deferred / Known Gaps

- `watch` command currently returns not implemented
- `tui` package is a stub
- No restart/stop/cache subcommands yet
- Service readiness checks are not modeled yet
- Example adapter is intentionally lightweight and not a full replacement for the target Nushell flows
