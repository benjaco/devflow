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
- Generic Docker-backed `pkg/database` module implemented for dedicated per-instance Postgres containers, ports, volumes, and snapshot/restore primitives
- Prisma schema-aware snapshot inspection and nearest migration-prefix restore planning implemented in `pkg/database`
- Generic dotenv loading implemented in `pkg/project`, and the example adapter now boots from `.env` with devflow-owned DB/runtime overrides layered on top
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
- Example adapter now structurally uses the dedicated DB flow: restore/reset, temporary runtime, migration replay, snapshot, then final `postgres` service
- Real `bikecoach` adapter added for an embedded-frontend + Go server workflow with dedicated per-worktree Postgres
- Unit/integration-style tests added for core packages
- Example-project smoke tests added for cache hits, watch reruns, and multi-worktree isolation
- Engine tests added for readiness success and readiness timeout failure
- Engine regression test added for group-tail scheduling so `KindGroup` targets do not falsely stall the scheduler
- Built-binary helper tests added for direct build/run/start behavior and engine-level cache restore
- Database module tests added for per-instance identity, runtime ensure behavior, and snapshot/restore planning
- Prisma snapshot tests added for exact-match and nearest-prefix restore selection
- Dotenv parser tests added, and example/CLI tests now verify runtime env and service logs include `.env` values while DB overrides still win
- CLI integration coverage added for the example adapter JSON lifecycle (`run`, `status`, `logs`, `instances`, `doctor`, `stop`)
- CLI regression test added so `run --json` still returns execution errors instead of swallowing them after emitting JSON
- Manual BikeCoach smoke coverage completed:
  - `doctor --json`
  - `graph list/show`
  - `run build-all --ci`
  - `run fullstack --ci` now fails early with a clear Docker-daemon-not-ready error if Docker is installed but not running
  - after starting Docker, `run fullstack --ci` succeeds end to end against the real BikeCoach repo
  - detached `run fullstack` starts the real backend and dedicated Postgres runtime
  - `/health` responds successfully on the assigned backend port
  - detached `stop --all` now leaves the status snapshot consistent with the stopped processes
- Detached operator UX improved:
  - detached runs launched via `go run` now re-exec through a copied stable launcher under `.devflow/bin`
  - supervisor logs are truncated per detached launch and available through `devflow logs supervisor`
  - `status` now reports instance/worktree/mode, derived local URLs, sanitized DB details, and detached supervisor liveness
  - `status` reconciles dead detached supervisors by clearing stale supervisor metadata and marking nonterminal nodes `stopped`
  - stopping an already-dead detached supervisor no longer errors with `no such process`
- First usable TUI slice implemented:
  - `devflow tui --worktree ...` opens a live operator console for an existing instance
  - live full-width task list with updating status and a log pane below is available
  - detached supervisor log can be viewed from inside the TUI
  - TUI rendering has unit coverage and manual BikeCoach smoke coverage
  - TUI renderer now avoids right-edge wrap corruption in VS Code terminals and shows explicit per-task state badges plus aggregate state counts
  - TUI task ordering now pins running work first, pending/ready work next, and keeps selection stable across refreshes
  - the original manual ANSI renderer was replaced with a `tview`-based implementation for stable redraws in real terminals
  - TUI now supports `i` on the selected task to invalidate the selected downstream cacheable slice and relaunch the current target; manually verified on BikeCoach by invalidating `build_coach`
  - TUI refresh cadence is now faster overall and much faster while invalidate/rerun actions are in flight
- Database runtime helper fixed so Docker combined-output errors are preserved and missing volume/container detection works against real daemon responses
- Database URL generation now appends `?sslmode=disable` for the dedicated local Postgres runtime
- Task-defined cache-key overrides are now implemented in the core task/runtime model
- Override-key unit coverage added in `pkg/fingerprint` and engine-level cache-restore coverage added in `pkg/engine`
- Opt-in real Docker-backed integration coverage added in `pkg/database` for:
  - dedicated Postgres runtime snapshot/restore
  - Prisma snapshot metadata plus nearest restore
- Verified locally with:
  - `go test ./...`
  - `DEVFLOW_E2E_DOCKER=1 go test ./pkg/database -run Docker -v`
- Added a second deterministic e2e example adapter at `examples/web-worker-workspace` covering:
  - API + worker + frontend services in one target
  - shared contract codegen with downstream service restarts
  - dedicated per-worktree DB prep/snapshot flow
  - multi-worktree port and DB isolation
  - watch-mode reruns across multiple long-running services
- Added a repo-local `devflow` launcher script plus bare-`devflow` CLI behavior:
  - builds the local binary into `.devflow/bin/devflow`
  - auto-detects the current worktree project
  - chooses the adapter default target
  - starts a detached run when needed
  - opens the TUI directly
- Installed `/usr/local/bin/devflow` as a symlink to the repo-local launcher on this machine
- Added watch pickup coverage in the deterministic example suite:
  - starts watch mode
  - edits a real input file
  - verifies the watch cycle event is emitted
  - verifies the affected service reruns
- Added a GitHub Actions workflow at `.github/workflows/build.yml` that:
  - sets up Go from `go.mod`
  - runs `go test ./...`
  - builds `./cmd/devflow`
- Updated the README to state clearly that GitHub Actions does not replace installing Go locally when you want to build or run `devflow` yourself
- Added focused task-hashing tests in `pkg/fingerprint` covering:
  - dependency key changes
  - input hash changes
  - env value changes
  - custom fingerprint changes
  - task-definition/signature changes
  - collected file-input hash changes after file edits
- Refined watch semantics:
  - watch-cycle events now report raw changed files in `files`
  - directly affected task names are now reported separately in `affectedTasks`
  - service-to-service watch restart propagation is now opt-in via `WatchRestartOnServiceDeps`
- Detached runs now persist engine events to `.devflow/state/instances/<instance-id>/events.jsonl`
- The TUI now uses the persisted event stream as its primary live-update trigger instead of relying on fast global polling
- Added regression coverage for:
  - watch event payload shape
  - service-to-service watch propagation defaults
  - detached event-stream persistence
- Execution now accepts a task name anywhere a target is accepted by wrapping the task as a synthetic single-root target
- The TUI now supports `t` on the selected task to retarget the detached run to that task and relaunch the instance on the selected task closure

## In Progress

- No active implementation in progress

## Next Steps

- Add richer watch restart policies now that service readiness exists
- Improve fine-grained detached service restart/control semantics beyond whole-target relaunch
- Expand TUI operator actions with confirmations and rerun/stop/restart controls
- Add stronger JSON contract tests for status/instances/events

## Deferred / Known Gaps

- Fine-grained detached per-service restart is not fully implemented yet
- The example adapter still uses a deterministic fake-DB path in normal tests; real Docker-backed coverage now exists as an opt-in module-level e2e layer rather than being part of default `go test ./...`
- The `bikecoach` adapter is now manually validated against the local repo for build, DB prep, detached runtime, health, and shutdown flows; remaining gaps are automated Docker-backed coverage and richer control UX
