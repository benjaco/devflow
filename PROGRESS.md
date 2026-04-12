# Progress

Last updated: 2026-04-13

## Current Status

- Phase: post-bootstrap core implementation
- State: graph/cache/process/instance/ports/engine/CLI foundation implemented and tested, with bounded parallel scheduling, a typed event stream, polling watch mode, basic operator commands, detached supervisor control, service readiness gates, and a realistic example adapter
- Confidence: core parallel/watch/operator/readiness paths are working; the example adapter is now a meaningful validation target; TUI and finer-grained detached service control are still pending

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
- Service readiness hooks implemented for service tasks, with generic ready-file/TCP/HTTP helpers and engine-enforced readiness timeouts
- Generic built-binary helper implemented in `pkg/project` for cacheable helper-binary builds plus later `Run`/`Start` execution
- Per-task cache-key override semantics documented for future implementation
- CLI commands implemented:
  - `run`
  - `watch`
  - detached `run/watch --detach`
  - `restart` for non-service task reruns
  - detached service `restart` via target relaunch
  - `stop`
  - `cache status`
  - `cache invalidate`
  - `cache gc`
  - `status`
  - `logs`
  - `instances`
  - `doctor`
  - `graph list`
  - `graph show`
  - `graph affected`
- `run --max-parallel` wired through the CLI and engine request model
- Example adapter added at `examples/go-next-monorepo`
- Example adapter upgraded to a deterministic full-stack-style workflow with DB prep, codegen, services, and watch semantics
- Unit/integration-style tests added for core packages
- Example-project smoke tests added for cache hits, watch reruns, and multi-worktree isolation
- Engine tests added for readiness success and readiness timeout failure
- Built-binary helper tests added for direct build/run/start behavior and engine-level cache restore
- CLI integration coverage added for the example adapter JSON lifecycle (`run`, `status`, `logs`, `instances`, `doctor`, `stop`)
- Verified with `go test ./...`

## In Progress

- No active implementation in progress

## Next Steps

- Build the first usable TUI slice in `pkg/tui`
- Add richer watch restart policies now that service readiness exists
- Implement task-defined cache-key overrides in the runtime/task model
- Improve fine-grained detached service restart/control semantics beyond whole-target relaunch
- Add detached-run smoke coverage for the example adapter

## Deferred / Known Gaps

- `tui` package is a stub
- Cache-key overrides are designed and documented but not implemented yet
- Fine-grained detached per-service restart is not fully implemented yet
- Example adapter is synthetic and local-only; it validates semantics but does not invoke real external tools or databases
