# Devflow

Devflow is a local-first DAG runner for development workflows.

It is designed for:
- cached one-shot tasks
- supervised long-running services
- adapter-defined service readiness checks
- detached background supervision for service-bearing runs
- a terminal UI for inspecting a live instance
- worktree-scoped instance isolation
- per-instance ports and runtime env
- watch-mode partial reruns and selective service restarts
- stable JSON output for humans, CI, and future agents

## Status

Early development. The current implementation focuses on the generic core:
- task graph validation and traversal
- content-based fingerprints
- local snapshot cache
- subprocess execution and service supervision
- service readiness gating before a service is considered running
- built-in binary-tool helpers for cacheable helper-binary builds and later execution
- Docker-backed database runtime helpers for dedicated per-instance Postgres containers and snapshots
- dotenv loading so adapter instances can start from `.env` and then apply devflow-owned overrides
- project-scoped dependency checks and platform-specific dependency installers
- detached `run/watch --detach` background supervisor launching
- `tui` for task selection and live log inspection
- worktree instances and shared port leasing
- polling-based watch mode using `github.com/radovskyb/watcher`
- JSON-capable CLI commands

The TUI is still minimal, but the bundled example adapters still act as real smoke targets for the core test suite:
- `go-next-monorepo` for the deterministic in-repo example workflow
- `web-worker-workspace` for a deterministic API + worker + frontend multi-service workflow
- `embedded-web-app` for a real embedded-frontend + Go server + dedicated Postgres workflow

## Quick Start

```bash
go build ./cmd/devflow
go test ./...
devflow
```

For real use, the target project repo should contain its own `devflow.project.go`.
The `devflow` launcher then compiles a worktree-local CLI and transfers execution into it.

If you want to use `devflow` locally, you still need Go installed on your machine.
GitHub Actions only verifies that the repo builds in CI; it does not remove the need for a local Go toolchain when you want to build or run the tool yourself.

The local flow is:
- install Go
- clone the repo
- run `go build ./cmd/devflow` or just use the repo-local `devflow` launcher script

Bare `devflow` is now a two-stage local launcher:
- it rebuilds the bootstrap `devflow` binary into the repo's `.devflow/bin/devflow` when core sources change
- when run inside a project worktree, it requires `./devflow.project.go`
- it compiles a worktree-local CLI into `<worktree>/.devflow/bin/devflow-local` when the project file or core sources are newer
- it `exec`s into that worktree-local CLI for all normal commands
- bare `devflow` in the project worktree then starts the project's default target detached if nothing is already running and opens the TUI

There is currently no built-in adapter fallback. If `devflow.project.go` is missing in the worktree, `devflow` fails.

Dependency management is also built in:
- `devflow deps status --project <name>` checks the adapter's required tool commands
- `devflow deps install --project <name>` runs platform-specific install scripts for missing tools
- `devflow doctor --project <name>` now reports missing adapter dependencies

## Design Goals

- generic standalone core
- adapter-defined project behavior
- human-first, agent-ready interfaces
- cache correctness before optimization
- service supervision separate from one-shot caching

## Roadmap

1. graph + cache
2. process + instance manager
3. watch mode
4. TUI
5. richer example adapter
6. MCP wrapper
