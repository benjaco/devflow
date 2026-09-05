# Roadmap

## Completed

- repo skeleton
- docs skeleton
- root `AGENTS.md`
- generic task and project model
- graph, fingerprint, cache, process, instance, ports, engine, and CLI foundations
- unit and integration coverage for the core
- bounded parallel engine scheduling
- typed event stream
- polling watch mode with selective reruns
- per-worktree daemon flow for mutable dev/watch/operator commands
- exclusive worktree execution leases shared by CI/daemon, serialized daemon transitions, cleanup-aware recovery and structured ownership conflicts
- first usable TUI with task/log panes and selected-task actions
- project-scoped required CLI checks and installers
- interactive prompt plumbing for prompt-driven subprocesses
- Docker-backed Postgres runtime helpers and snapshot planning
- project-local `devflow.project.go` bootstrap flow
- `devflow flush [target]` readiness gate for detached watch workflows
- Go-first install/update flow with `go install` and `devflow upgrade`
- global OS user task cache with project namespaces
- explicit documentation split between project adoption and Devflow contributor workflows
- per-worktree localbuild locking for concurrent project-local binary builds
- reliable `stop --all` cleanup for daemon-owned work, recorded service process groups, and stale status PIDs
- explicit service lifecycle contract for attached run, CI readiness probes, detached run/watch, flush, status, and stop
- graph affected explanations plus aligned ignore semantics for watch matching and fingerprinting
- target-scoped required CLI declarations plus `doctor --target <target> --json`
- deterministic example adapters plus a real embedded-web-app adapter
- GitHub Actions build/test workflow
- supported Go 1.27.1 baseline with rolling current-stable Linux compatibility, cross-platform quality/security checks, a Linux race-detector gate, and monthly dependency update checks
- finite pipeline validation with projected input/output sandboxes and exhaustive dependency-valid sequential order execution
- shared bounded cache/validation filesystem projection with read-only directory handling and safe internal symlink preservation
- credential-safe host and containerized PostgreSQL clone clients
- CI stderr progress plus self-contained failure JSON with node/cache timings and bounded log tails
- target-scoped required-env doctor checks, explicit env precedence, cache key/path introspection, and long-line scanner hardening

## Next Milestones

The CM Navigator agent-integration review has an approved engineering sequence in [Agent verification: assessment and implementation plan](agent-verification-plan.md). Item 1 (execution ownership), its current-only revision and CI correction are accepted. Item 2 (startup/flush freshness) is implemented and under verification for review; see [regression evidence](watch-freshness-verification.md). Successful upgrades discard the shared task artifact cache; future work targets the current API/state contracts directly. Continue one item at a time after review; the original assessment baseline is `a7fe4f8`.

1. Review startup/flush freshness, then share finite task lifecycle with validation as item 3.
2. Repair log following and JSON error paths; propagate direct execution cancellation.
3. Introduce consistent run/attempt identities, retained evidence, scoped cancellation and a recoverable prompt protocol.
4. Expose existing graph metadata and add a conservative verification planner using declared purposes/effects and bootstrap-aware configuration-change detection.
5. Add opt-in compact results, progress control and cursor retrieval; defer broader same-worktree concurrency and MCP until the underlying contracts are proven.

The earlier BikeCoach adoption work remains useful alongside that sequence:

1. User adoption docs and examples
   - Add a full "converge from scripts to Devflow" user guide based on the BikeCoach integration pattern.
   - Expand the managed database APIs into a complete example that starts a container, waits for host readiness, creates/prepares the database, runs migrations, preserves prefix snapshots, and stops cleanly.
   - Add a fixed-port service example for apps that need stable callback URLs such as OAuth redirects.
   - Add first-class DB snapshot activity visibility in JSON/events so adapters do not need custom summary logging for restored snapshot, prefix length, applied count, and final snapshot key.

2. Broader operator surface
   - Add fine-grained detached service restart/control beyond whole-target relaunch once process cleanup and lifecycle contracts are solid.
   - Expand TUI operator actions with confirmations and rerun/stop/restart controls after the CLI behavior is stable.
   - Add stronger JSON contract tests for status, instances, events, and flush.
   - Build an MCP wrapper over the stable CLI surface after the real-project workflow is smoother.

3. Go debug-service hardening
   - Validate VS Code/Cursor attach behavior against the `NodeStatus.Debug` attach metadata.
   - Consider replacing Windows `taskkill /T /F` cleanup with Job Objects if real repeated Delve restarts show locked binaries or orphaned debuggee processes.
   - Defer attach-to-existing-process workflows, editor-driven DAP restart orchestration, and legacy JSON-RPC attach until the owned `dlv exec` path is proven in real projects.

## Feedback Disposition

- Completed from BikeCoach feedback: per-worktree localbuild locking for concurrent CLI commands.
- Completed from BikeCoach feedback: reliable `stop --all` cleanup for daemon-owned work, tracked services, and recorded status process groups.
- Completed from BikeCoach feedback: service lifecycle contract documentation plus CI-mode service readiness probes that stop services before returning.
- Completed from BikeCoach feedback: watch/debug ergonomics via `graph affected --explain` and aligned root-relative/directory-relative ignore matching between watch and fingerprinting.
- Completed from BikeCoach feedback: target-scoped required CLI declarations plus `doctor --target <target> --json`.
- Completed from BikeCoach feedback: managed Postgres host-port readiness, stale published-port reconciliation, `run --help` flag descriptions, finite service-dependent target guidance, and secret/runtime-env documentation.
- Completed from Prisma/Postgres adoption test: Prisma migration inspection ignores `migration_lock.toml`, fresh schemas with models fail before smoke tests when no migrations exist, remote clone failures from `pg_dump` are not masked, `stop --all` stops the managed DB container, and first task errors are preserved over sibling cancellation noise.
- Completed startup/flush correction: establish the observer before the initial DAG and bind flush to the captured watch plus a fresh reconciliation boundary. This replaces the earlier sentinel-retouch workaround and also covers edits during rebuilding and health probes.
- Completed from reworked Prisma/Postgres setup feedback: the builder/component API is the preferred adapter shape, `prisma.NewMigration(b)` now reconciles the managed database before authoring so edited latest migrations do not force a manual reset, and docs clarify component task names, consumer-owned target names, `pg_dump` major-version compatibility, and stopped database metadata.
- Completed from real-project CI/cache feedback: cache and validation now share a bounded copier that preserves safe relative links without expanding pnpm graphs, repairs read-only cleanup, and reports copy progress; CI JSON runs stream progress to stderr and return failure text/log context plus node/cache timing; scanner errors are propagated with a 4 MiB line bound.
- Completed from real-project security/config feedback: PostgreSQL host clients use owner-only pgpass files and password-free arguments, containerized clients are available through `CloneFromEnvContainerized`, task/daemon/event logs are private, process env overrides dotenv defaults for declared keys while managed runtime values win last, and `RequiredEnv` plus `doctor --strict` makes missing inputs actionable.
- Completed from real-project cache integration feedback: `cache key --target ... --json` and `cache path --json` expose the supported aggregate fingerprint and physical namespace path.
- Completed from Delve research: first-class `GoDebugService` builds a stable debug binary with `go build -gcflags=all=-N -l`, runs `dlv exec` headless on a stable local debug port, exposes debug attach metadata in status JSON, participates in daemon/watch stop-rebuild-relaunch, and has fake `go`/`dlv` lifecycle coverage plus real Delve CI-mode smoke coverage installed in the OS matrix.
- Accepted as immediate roadmap input: complete script-convergence docs, a full managed Postgres example, and fixed-port examples.
- Reframed: "service target `run` returns after readiness" should not silently change attached `run` semantics. The current automation path is `watch --detach` plus `flush`; CI mode can probe readiness but stops services before returning.
- Reframed: a fixed-port HTTP readiness helper should probably be part of broader env-aware readiness patterns rather than a BikeCoach-specific helper.
- Deferred behind reliability work: fine-grained service restart/control, TUI restart/stop actions, MCP wrapper, and richer installer channels.
- Optional publishing of Devflow-maintained multi-architecture PostGIS images remains deferred. The architecture-aware native arm64 build is correct and cached locally; publishing images adds registry ownership, release automation, provenance, and patch cadence that should be designed as a separate release concern.
