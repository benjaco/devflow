# Agent Memory

This is the shared long-term memory for AI work on Devflow.

Use it on every substantial agent session to preserve project health across model changes, context resets, and separate coding runs. It is the common project brain for stable operating principles, current shape, recurring constraints, and active risks that should not live only in chat.

## Read Order

1. `AGENTS.md` for project rules and non-goals.
2. `docs_contributors/agent-memory.md` for shared project memory and long-term AI operating context.
3. `PROGRESS.md` for the current implementation ledger, active milestone, next steps, and known gaps.
4. `docs_contributors/architecture.md` for package boundaries and runtime state layout.
5. `docs_contributors/cli.md` for user-facing command behavior and JSON surfaces.
6. `docs_contributors/testing.md` before changing behavior with broad blast radius.
7. `docs_contributors/README.md` for contributor workflow and `docs_users/README.md` for user/adopter workflow.

## Memory Policy

- Read this file before substantial work, not only when inheriting from another agent.
- Update this file when an implementation decision changes how future agents should think.
- Keep transient task history in `PROGRESS.md`; keep durable mental models and constraints here.
- Keep subsystem facts in the relevant subsystem docs; use this file to connect the cross-cutting implications future agents need to remember.
- Do not duplicate every changelog entry. Store only context that protects future correctness, maintainability, or product direction.
- If chat contains important project context, move the durable part into docs before ending the work.

## Subsystem Documentation Links

Use this memory together with the subsystem docs. When a change affects one of these areas, update the corresponding doc rather than only editing this file.

- `docs_contributors/architecture.md`: package boundaries, runtime state layout, bootstrap flow, cache/env/database design
- `docs_contributors/cli.md`: command behavior, JSON output contracts, TUI/operator semantics
- `docs_users/README.md`: user-facing workflow for adding Devflow to another project
- `docs_contributors/README.md`: contributor workflow for changing Devflow itself
- `docs_users/adapter-guide.md`: adapter authoring expectations and project-local behavior
- `docs_users/agent-integration.md`: agent-facing execution surfaces and future wrapper direction
- `docs_contributors/testing.md`: default and opt-in verification expectations
- `docs_contributors/roadmap.md`: active priorities and deferred work

## Working Mindset

- Keep the core generic. Project-specific behavior belongs in adapters, examples, or project-local `devflow.project.go` files.
- Keep documentation split into two lanes: user/adopter docs for applying Devflow in another project, and contributor docs for changing Devflow itself.
- Preserve stable JSON output for every user-facing command except `devflow docs`, which intentionally prints plain bundled user Markdown only.
- Treat worktrees as the isolation boundary.
- Devflow itself does not need to be developed through git worktrees; worktree support is for target projects.
- Go is required on machines that run Devflow because project graph definitions are Go code. Round-1 install/update is `go install github.com/benjaco/devflow/cmd/devflow@latest` and `devflow upgrade`; binary releases are deliberately deferred.
- Keep instance env explicit, layered, and persisted.
- Services are supervised, not cached.
- Cacheable tasks must declare outputs.
- Keep task cache storage in one OS user cache folder (`<os.UserCacheDir()>/devflow/cache`) and namespace entries by project. Per-worktree logs/state stay in the worktree `.devflow/`.
- Prefer narrow, semantic fingerprints over hashing the whole repo.
- Optimize cache storage only after correctness and contract coverage exist.
- Watch reruns must preserve graph dependency barriers. If an intermediate task is blocked from running in watch mode, downstream tasks in that cascade must not run against stale intermediate outputs.
- Watch service restart policies should stay explicit: default to affected-slice restarts, use `RestartNever` to block watch restarts, and reserve `RestartAlways` for services that must bounce on every target-affecting watch cycle.
- Treat `devflow flush --json` as the AI readiness gate for detached watch/dev workflows: edit files, flush the selected target closure, then run tests only when flush reports success.

## Current Shape

Devflow is now beyond the initial bootstrap. The core includes graph validation, fingerprinting, snapshot caching, process supervision, instance and port state, bounded parallel engine scheduling, typed events, polling watch mode, flush readiness coordination, required CLI checks/installers, interactive prompt plumbing, a TUI, and Docker-backed Postgres runtime helpers.

Runtime adapters are project-local:
- installed `devflow` or the repo-level source launcher builds/uses the bootstrap binary
- a selected worktree must contain `devflow.project.go`
- the bootstrap CLI compiles `<worktree>/.devflow/bin/devflow-local`
- project-local binary stale checks and rebuilds are serialized by `<worktree>/.devflow/localbuild.lock`
- normal commands exec into that worktree-local binary
- there is no built-in adapter fallback when `devflow.project.go` is missing

The first real BikeCoach integration showed that adoption hardening is now more important than expanding operator features in the abstract. After localbuild locking, reliable detached cleanup, explicit service lifecycle contracts, graph affected explanations, target-scoped required CLI checks, and managed Postgres host-port hardening, the highest-value issues are complete user examples for script convergence, fixed ports, and managed Postgres.

Service lifecycle contract: attached `run` is foreground and blocks while services live; `run --ci` may start services as readiness probes but stops them before returning; `run --detach` only proves supervisor launch; `watch --detach` plus `flush` is the readiness-gated background workflow for humans and agents.

Watch/input debugging contract: `Inputs.Ignore` is shared by fingerprinting and watch matching. Patterns are slash-normalized, checked root-relative, and for directory inputs also checked relative to the input dir. `devflow graph affected --files <path> --explain --json` is the first tool to use when generated files cause surprising watch cascades.

Required CLI contract: `RequiredCLIs()` is the project catalog. `RequiredCLIs` on tasks and targets selects the subset needed for a target closure. `devflow doctor --target <target> --json` and `devflow clis status/install --target <target>` must not report unrelated catalog entries. The older `Dependencies()` provider remains only as a compatibility path.

Managed Postgres contract: app code connects through the host-mapped port, so database readiness must include host-port readiness, not only in-container `pg_isready`. `EnsureRuntime` should preserve volumes but recreate stale containers whose published port no longer matches the current instance. Migration snapshots must be prefix-safe: restore only exact/prefix-compatible snapshots, and use `ApplyEach` or the default Prisma workflow when a workflow needs cached intermediate migration points so editing the latest migration can restore the previous prefix.

Prisma workflow contract: normal DB preparation applies migrations with `prisma migrate deploy` and snapshots prefixes; migration authoring is separate. If `schema.prisma` changes without a new migration, the default Prisma workflow should fail with an explicit "generate a migration" error instead of masking drift during `up`.

Runtime env and secrets: instance env is persisted under `.devflow/state`. Adapters should avoid storing long-lived production secrets there, avoid logging whole env maps, and override runtime values such as `PORT` for unit-test tasks when those tests should not inherit service runtime ports.

State is split deliberately:
- per-worktree logs and instance snapshots live under the worktree `.devflow/`
- all projects share one physical task cache under the OS user cache dir, with entries namespaced by project
- sibling git worktrees share port allocation through the Git common dir
- non-git temp/test flows fall back to local/global safe defaults

## Before Editing

- Check `git status --short` and preserve any user changes.
- Update `PROGRESS.md` at the start and end of substantial work.
- Prefer existing package boundaries and helper APIs over new abstractions.
- Keep public behavior documented when changing CLI output, adapter contracts, or runtime state.
- Avoid hidden interactive subprocesses in normal boot/watch paths; model destructive or ambiguous choices as explicit actions.

## Verification Baseline

Default verification:

```bash
go test ./...
```

Useful command smoke checks:

```bash
go run ./cmd/devflow doctor --json
go run ./cmd/devflow status --json
go run ./cmd/devflow run <target> --json --project <name>
```

Docker-backed database integration tests are opt-in and should be run when changing `pkg/database` runtime behavior:

```bash
DEVFLOW_E2E_DOCKER=1 go test ./pkg/database -run Docker -v
```

## Current Priorities

The latest concrete next steps are maintained in `PROGRESS.md`. As of this memory update, likely next work includes:

- user docs/examples for script-to-Devflow convergence, a complete managed local Postgres example, and fixed ports

## Deliberate Deferrals

- Do not introduce a YAML-first config DSL in v1.
- Do not hardcode Prisma, sqlc, Next.js, or repo-specific paths into core packages.
- Do not replace adapter-owned database base-source policy with implicit reset behavior.
- Do not make real Docker e2e tests part of default `go test ./...` until the project intentionally changes that contract.
