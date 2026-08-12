# Overview

Devflow is a local-first runner for development DAGs. It executes cacheable one-shot tasks, supervises long-running services, isolates concurrent worktrees, and exposes stable JSON output so the same surface can serve humans, CI, and later agent wrappers.

Documentation is split into two explicit lanes:
- `docs_users/setup.md` and `docs_users/adapter-guide.md` are for users adding Devflow to another project.
- `docs_users/development.md` and `docs_users/agent-integration.md` are for day-to-day project operation after Devflow is integrated.
- `docs_contributors/README.md`, `docs_contributors/architecture.md`, and `docs_contributors/testing.md` are for contributors changing Devflow itself.

The current implementation provides the generic engine layers first:
- graph validation and traversal
- task fingerprinting
- snapshot-based local cache
- process supervision
- instance and port management
- finite sandbox validation of task input/output contracts and every valid sequential task order
- one daemon per worktree for mutable dev/watch/operator workflows
- a dedicated Postgres runtime module for per-worktree container isolation
- JSON CLI contracts

Current milestones focus on adoption hardening, richer database activity visibility, examples, and the remaining fine-grained operator controls.

Current bundled adapters cover three distinct validation shapes:
- `go-next-monorepo`: deterministic in-repo example for repeatable tests
- `web-worker-workspace`: deterministic multi-service API + worker + frontend example
- `embedded-web-app`: real Go server with embedded Vite frontends plus dedicated per-worktree Postgres
