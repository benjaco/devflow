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
- deterministic example adapters plus a real embedded-web-app adapter
- GitHub Actions build/test workflow

## Next Milestones

1. Richer watch restart policies now that service readiness exists
2. Fine-grained detached service restart/control beyond whole-target relaunch
3. Project-local adapter loading beyond a single self-contained `devflow.project.go`
4. Broader TUI operator actions with confirmations and rerun/stop/restart controls
5. Stronger JSON contract tests for status, instances, and events
6. MCP wrapper over the stable CLI surface
