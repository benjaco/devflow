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
- detached supervisor flow
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
- reliable `stop --all` cleanup for detached supervisors, child executors, service process groups, and stale status PIDs
- explicit service lifecycle contract for attached run, CI readiness probes, detached run/watch, flush, status, and stop
- graph affected explanations plus aligned ignore semantics for watch matching and fingerprinting
- target-scoped required CLI declarations plus `doctor --target <target> --json`
- deterministic example adapters plus a real embedded-web-app adapter
- GitHub Actions build/test workflow

## Next Milestones

The BikeCoach real-project integration moved the next focus from generic operator expansion to adoption hardening. The next work should make the installed CLI safe and understandable in a real repository where humans and agents call commands quickly, stale detached state can exist, and the current workflow may still be script-based.

1. User adoption docs and examples
   - Add a full "converge from scripts to Devflow" user guide based on the BikeCoach integration pattern.
   - Expand the managed database APIs into a complete example that starts a container, waits for host readiness, creates/prepares the database, runs migrations, preserves prefix snapshots, and stops cleanly.
   - Add a fixed-port service example for apps that need stable callback URLs such as OAuth redirects.

2. Broader operator surface
   - Add fine-grained detached service restart/control beyond whole-target relaunch once process cleanup and lifecycle contracts are solid.
   - Expand TUI operator actions with confirmations and rerun/stop/restart controls after the CLI behavior is stable.
   - Add stronger JSON contract tests for status, instances, events, and flush.
   - Build an MCP wrapper over the stable CLI surface after the real-project workflow is smoother.

## Feedback Disposition

- Completed from BikeCoach feedback: per-worktree localbuild locking for concurrent CLI commands.
- Completed from BikeCoach feedback: reliable `stop --all` cleanup for detached supervisors, child executors, tracked services, and stale status process groups.
- Completed from BikeCoach feedback: service lifecycle contract documentation plus CI-mode service readiness probes that stop services before returning.
- Completed from BikeCoach feedback: watch/debug ergonomics via `graph affected --explain` and aligned root-relative/directory-relative ignore matching between watch and fingerprinting.
- Completed from BikeCoach feedback: target-scoped required CLI declarations plus `doctor --target <target> --json`.
- Completed from BikeCoach feedback: managed Postgres host-port readiness, stale published-port reconciliation, `run --help` flag descriptions, finite service-dependent target guidance, and secret/runtime-env documentation.
- Completed from Prisma/Postgres adoption test: Prisma migration inspection ignores `migration_lock.toml`, fresh schemas with models fail before smoke tests when no migrations exist, remote clone failures from `pg_dump` are not masked, `stop --all` stops the managed DB container, and first task errors are preserved over sibling cancellation noise.
- Accepted as immediate roadmap input: complete script-convergence docs, a full managed Postgres example, and fixed-port examples.
- Reframed: "service target `run` returns after readiness" should not silently change attached `run` semantics. The current automation path is `watch --detach` plus `flush`; CI mode can probe readiness but stops services before returning.
- Reframed: a fixed-port HTTP readiness helper should probably be part of broader env-aware readiness patterns rather than a BikeCoach-specific helper.
- Deferred behind reliability work: fine-grained service restart/control, TUI restart/stop actions, MCP wrapper, and richer installer channels.
