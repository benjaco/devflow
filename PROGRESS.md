# Progress

Last updated: 2026-04-12

## Current Status

- Phase: post-bootstrap core implementation
- State: graph/cache/process/instance/ports/engine/CLI foundation implemented and tested, with bounded parallel scheduling, a typed event stream, and polling watch mode
- Confidence: core parallel and watch paths are working; TUI and richer operator controls are still pending

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
- Polling watch mode implemented with debounced batches and selective reruns via `github.com/radovskyb/watcher`
- CLI commands implemented:
  - `run`
  - `watch`
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

- Expand CLI coverage for restart/stop/cache subcommands
- Build the first usable TUI slice in `pkg/tui`
- Replace the placeholder example adapter behavior with a more realistic adapter contract exercise
- Add service readiness checks and richer watch restart policies

## Deferred / Known Gaps

- `tui` package is a stub
- No restart/stop/cache subcommands yet
- Service readiness checks are not modeled yet
- Example adapter is intentionally lightweight and not a full replacement for the target Nushell flows
