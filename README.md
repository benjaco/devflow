# Devflow

Devflow is a local-first DAG runner for development workflows.

It is designed for:
- cached one-shot tasks
- supervised long-running services
- worktree-scoped instance isolation
- per-instance ports and runtime env
- stable JSON output for humans, CI, and future agents

## Status

Early development. The current implementation focuses on the generic core:
- task graph validation and traversal
- content-based fingerprints
- local snapshot cache
- subprocess execution and service supervision
- worktree instances and shared port leasing
- JSON-capable CLI commands

Watch mode, the TUI, and the full-stack example adapter are scaffolded but intentionally minimal until the core is stable.

## Quick Start

```bash
go test ./...
go run ./cmd/devflow graph list --project go-next-monorepo
go run ./cmd/devflow run fullstack --project go-next-monorepo --json
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
