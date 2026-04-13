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
- `bikecoach` for a real embedded-frontend + Go server + dedicated Postgres workflow

## Quick Start

```bash
go test ./...
go run ./cmd/devflow graph list --project go-next-monorepo
go run ./cmd/devflow run fullstack --project go-next-monorepo --worktree examples/go-next-monorepo/worktree --json
go run ./cmd/devflow run build-all --project bikecoach --worktree /path/to/bikecoach/coach --json --ci
```

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
