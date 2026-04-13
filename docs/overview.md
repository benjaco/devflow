# Overview

Devflow is a local-first runner for development DAGs. It executes cacheable one-shot tasks, supervises long-running services, isolates concurrent worktrees, and exposes stable JSON output so the same surface can serve humans, CI, and later agent wrappers.

The current implementation provides the generic engine layers first:
- graph validation and traversal
- task fingerprinting
- snapshot-based local cache
- process supervision
- instance and port management
- a dedicated Postgres runtime module for per-worktree container isolation
- JSON CLI contracts

Future milestones add a richer TUI and continue expanding the example adapter and operator surface.

Current bundled adapters cover two distinct validation shapes:
- `go-next-monorepo`: deterministic in-repo example for repeatable tests
- `bikecoach`: real Go server with embedded Vite frontends plus dedicated per-worktree Postgres
