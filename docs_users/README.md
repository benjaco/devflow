# Devflow User Docs

Devflow user documentation is intentionally split so humans and agents can fetch only the context they need.

## Setup Docs

Use setup docs when adding Devflow to a project, shaping the pipeline, or writing `devflow.project.go`.

```bash
devflow docs setup
```

This includes:
- installing/upgrading Devflow
- adding `.devflow/` and `devflow.project.go`
- defining tasks, services, targets, inputs, and outputs
- required CLI declarations
- managed Postgres and Prisma setup patterns
- what to commit

## Development Docs

Use development docs when Devflow is already integrated and you need to run the project day to day.

```bash
devflow docs development
```

This includes:
- CLI usage
- TUI controls
- watch/flush loops
- status and logs
- doctor and required CLI checks
- cache operations
- runtime state layout

## Contributor Docs

These user docs are not for changing Devflow itself. For contributor work in this repository, use `docs_contributors/README.md`, `docs_contributors/agent-memory.md`, and `PROGRESS.md`.
