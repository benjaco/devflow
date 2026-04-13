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
- detached `run/watch --detach` background supervisor launching
- `tui` for task selection and live log inspection
- worktree instances and shared port leasing
- polling-based watch mode using `github.com/radovskyb/watcher`
- JSON-capable CLI commands

The TUI is still minimal, but the bundled adapters now act as real smoke targets for the core:
- `go-next-monorepo` for the deterministic in-repo example workflow
- `web-worker-workspace` for a deterministic API + worker + frontend multi-service workflow
- `embedded-web-app` for a real embedded-frontend + Go server + dedicated Postgres workflow

## Quick Start

```bash
go build ./cmd/devflow
go test ./...
devflow
go run ./cmd/devflow graph list --project go-next-monorepo
go run ./cmd/devflow run fullstack --project go-next-monorepo --worktree examples/go-next-monorepo/worktree --json
go run ./cmd/devflow run build-all --project embedded-web-app --worktree /path/to/project --json --ci
```

If you want to use `devflow` locally, you still need Go installed on your machine.
GitHub Actions only verifies that the repo builds in CI; it does not remove the need for a local Go toolchain when you want to build or run the tool yourself.

The local flow is:
- install Go
- clone the repo
- run `go build ./cmd/devflow` or just use the repo-local `devflow` launcher script

Bare `devflow` is now a local launcher:
- it rebuilds the local `devflow` binary into `.devflow/bin/devflow` when sources change
- it detects the current worktree project
- it starts the project's default target detached if nothing is already running
- it opens the TUI for that worktree

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
