# Overview

Devflow is a local-first runner for development DAGs. It executes cacheable one-shot tasks, supervises long-running services, isolates concurrent worktrees, and exposes stable JSON output so the same surface can serve humans, CI, and later agent wrappers.

Documentation is split into two explicit lanes:
- `docs_users/README.md` and `docs_users/adapter-guide.md` are for users adding Devflow to another project.
- `docs_contributors/README.md`, `docs_contributors/architecture.md`, and `docs_contributors/testing.md` are for contributors changing Devflow itself.

The current implementation provides the generic engine layers first:
- graph validation and traversal
- task fingerprinting
- snapshot-based local cache
- process supervision
- instance and port management
- a dedicated Postgres runtime module for per-worktree container isolation
- JSON CLI contracts

Current milestones focus on making the existing TUI, detached supervisor controls, watch restart policies, JSON contracts, and project-local adapter loading more complete.

Current bundled adapters cover three distinct validation shapes:
- `go-next-monorepo`: deterministic in-repo example for repeatable tests
- `web-worker-workspace`: deterministic multi-service API + worker + frontend example
- `embedded-web-app`: real Go server with embedded Vite frontends plus dedicated per-worktree Postgres
