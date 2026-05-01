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
- project-scoped dependency checks and installers
- interactive prompt plumbing for prompt-driven subprocesses
- Docker-backed Postgres runtime helpers and snapshot planning
- project-local `devflow.project.go` bootstrap flow
- `devflow flush [target]` readiness gate for detached watch workflows
- Go-first install/update flow with `go install` and `devflow upgrade`
- global OS user task cache with project namespaces
- explicit documentation split between project adoption and Devflow contributor workflows
- per-worktree localbuild locking for concurrent project-local binary builds
- deterministic example adapters plus a real embedded-web-app adapter
- GitHub Actions build/test workflow

## Next Milestones

The BikeCoach real-project integration moved the next focus from generic operator expansion to adoption hardening. The next work should make the installed CLI safe and understandable in a real repository where humans and agents call commands quickly, stale detached state can exist, and the current workflow may still be script-based.

1. Detached cleanup reliability
   - Make `stop --all` reconcile and terminate all known detached supervisors, `__internal_exec` children, service processes, and stale process groups for the selected worktree.
   - Add regression coverage for stale detached watch cleanup.

2. Service lifecycle contracts
   - Document the difference between attached `run` of a service target, detached `run/watch`, `flush`, `status`, and `stop`.
   - Decide whether service-target `run` should keep its attached semantics only, or add a first-class mode that returns after readiness for automation.
   - Keep fine-grained service restart/control on the roadmap, but build it after stop/reconciliation behavior is reliable.

3. Watch and graph explainability
   - Make input ignore semantics consistent between fingerprinting and watch matching, or document the difference explicitly.
   - Add tooling that explains why a changed file affects a task or target, building on `graph affected`.
   - Improve generated-output ergonomics so adapters can prevent watch loops without fragile pattern rules.

4. Target-scoped dependencies and doctor
   - Allow dependency declarations to attach to tasks or targets, not only the whole project.
   - Add `doctor --target <target> --json` so developers and agents see only dependency problems relevant to the selected target closure.

5. User adoption docs and examples
   - Add a full "converge from scripts to Devflow" user guide based on the BikeCoach integration pattern.
   - Add a complete managed local Postgres example that starts a container, waits for readiness, creates/prepares the database, runs migrations, preserves or snapshots state, and stops cleanly.
   - Add a fixed-port service example for apps that need stable callback URLs such as OAuth redirects.
   - Document secret handling: what may be persisted under `.devflow/state`, what may appear in logs/status, and how adapters should avoid or redact sensitive values.

6. Broader operator surface
   - Add fine-grained detached service restart/control beyond whole-target relaunch once process cleanup and lifecycle contracts are solid.
   - Expand TUI operator actions with confirmations and rerun/stop/restart controls after the CLI behavior is stable.
   - Add stronger JSON contract tests for status, instances, events, and flush.
   - Build an MCP wrapper over the stable CLI surface after the real-project workflow is smoother.

## Feedback Disposition

- Completed from BikeCoach feedback: per-worktree localbuild locking for concurrent CLI commands.
- Accepted as immediate roadmap input: reliable `stop --all`, service lifecycle docs, watch/debug ergonomics, target-scoped dependencies, managed Postgres docs/examples, fixed-port examples, and secret-handling guidance.
- Reframed: "service target `run` returns after readiness" should not silently change attached `run` semantics without a deliberate CLI contract. Prefer either explicit docs or a new/clear mode for automation.
- Reframed: a fixed-port HTTP readiness helper should probably be part of broader env-aware readiness patterns rather than a BikeCoach-specific helper.
- Deferred behind reliability work: fine-grained service restart/control, TUI restart/stop actions, MCP wrapper, and richer installer channels.
