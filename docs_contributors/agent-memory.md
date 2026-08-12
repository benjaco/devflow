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
7. `docs_contributors/README.md` for contributor workflow, `docs_users/setup.md` for user setup, and `docs_users/development.md` for day-to-day user operation.

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
- `docs_users/setup.md`: setup/pipeline workflow for adding Devflow to another project
- `docs_users/development.md`: day-to-day CLI/TUI/operator workflow after Devflow is integrated
- `docs_contributors/README.md`: contributor workflow for changing Devflow itself
- `docs_users/adapter-guide.md`: adapter authoring expectations and project-local behavior
- `docs_users/agent-integration.md`: agent-facing execution surfaces and future wrapper direction
- `docs_contributors/testing.md`: default and opt-in verification expectations
- `docs_contributors/roadmap.md`: active priorities and deferred work

## Working Mindset

- Keep the core generic. Project-specific behavior belongs in adapters, examples, or project-local `devflow.project.go` files.
- Keep documentation split into contributor docs and scoped user docs. User docs are further split into setup/pipeline context and day-to-day development/operator context so agents do not have to ingest both.
- Preserve stable JSON output for every user-facing command except `devflow docs setup` and `devflow docs development`, which intentionally print scoped plain bundled user Markdown only. Bare `devflow docs` should stay a usage error to prevent context-heavy all-doc dumps.
- Treat worktrees as the isolation boundary.
- Devflow itself does not need to be developed through git worktrees; worktree support is for target projects.
- Go is required on machines that run Devflow because project graph definitions are Go code. Round-1 install/update is `go install github.com/benjaco/devflow/cmd/devflow@latest` and `devflow upgrade`; binary releases are deliberately deferred.
- `devflow upgrade` should keep the normal Go proxy path for ordinary user updates. `devflow upgrade --direct` exists for testing freshly pushed commits before the public Go proxy catches up. Upgrade only updates the binary written by `go install`; if a repo-local launcher or another `devflow` command shadows `$(go env GOPATH)/bin/devflow` earlier on `PATH`, the shell will keep running the shadowing command, so text-mode upgrade should warn about that.
- Go 1.25 is the supported minimum. Default CI tests Go 1.25 on Linux amd64, macOS 15 arm64, and Windows amd64, asserts each native host architecture, tests the current stable Go release on Linux, runs the race detector on Linux, and enforces formatting, tidy module metadata, vet, Staticcheck, and govulncheck. Keep tests and helpers portable by default: use generated Go helper binaries instead of Unix shell-script fake tools, add `.exe` to built helper paths on Windows, put platform-specific process/lock behavior behind build-tagged files, and use structured encoders for JSON fixtures containing filesystem paths.
- On Windows, every copied or generated executable path under `.devflow/bin` must include the `.exe` suffix, including worktree-local `devflow-local` and daemon helper binaries. Building a PE file to an extensionless path is not enough for reliable `os/exec` startup.
- Cross-platform tests that assert cache miss/hit counts must isolate all OS cache env vars they depend on: `HOME`, `XDG_CACHE_HOME`, and `LOCALAPPDATA`. Windows `os.UserCacheDir()` uses `LOCALAPPDATA`, so setting only `HOME` can leak task cache state between tests or CI attempts.
- Source-local bootstrap must not require a project `devflow.project.go` to be relative to the Devflow source checkout. On Windows, temp worktrees can be on a different volume than the repo, so local build hashing needs a stable external source label instead of failing `filepath.Rel`.
- Windows cannot replace the current process with `syscall.Exec`. Local-project bootstrap on Windows must run the worktree-local binary as a child and exit with the child's code, while Unix can keep using process replacement.
- Windows process cleanup must terminate process trees, not only the immediate parent process. Service tasks and `go run`-style helpers can leave children holding stdout/stderr pipes and task log files open if only the parent is killed, which breaks watch settling and temp-dir cleanup.
- Keep instance env explicit, layered, and persisted.
- Persisted JSON state and `runtime.env` use unique same-directory temporary files before replacement and owner-only file permissions on Unix-like systems. Preserve this: fixed `.tmp` paths race under concurrent writers, direct truncation can expose partial state, and these files can contain local credentials even though they are not encrypted secret storage.
- Services are supervised, not cached.
- Mutable dev/watch/operator work is owned by one daemon per worktree. CLI and TUI commands should connect to that daemon for `run` (non-CI), `watch`, `flush`, `restart`, `stop`, retarget, invalidate/rerun, and Prisma migration authoring. `run --ci` is the deliberate exception: it remains direct and finite so CI-style checks never depend on a long-lived daemon.
- Daemon startup is protected by a per-instance file lock under worktree state. Do not bypass `daemon.Ensure` for user-facing mutable commands, or concurrent CLI/TUI invocations can violate the one-daemon-per-worktree invariant.
- A running daemon must be replaced when the repo-local/project-local executable changes; otherwise local unpushed testing can keep talking to an old daemon. `stop --all` should stop active work and then shut the daemon down. `status` must stay read-only and must not start a daemon just to inspect stopped state.
- Task logs are per-attempt. Truncate at the engine task-attempt boundary, not inside adapter/component progress helpers. Subprocess logging appends within that attempt so custom `EmitLogLine` progress and command output do not erase each other, and stale failure output does not appear under a newer running state. Runtime component helpers that pass subprocess output through `process.Run` should use an event-only line callback after the process runner writes the log; otherwise task logs and the TUI can show duplicated Prisma/npm output.
- TUI daemon ownership is session-scoped: if bare `devflow` or `devflow tui` creates the daemon for that TUI session, exiting the TUI should send the normal all-work stop request and shut the daemon down; if the daemon already existed, TUI exit must only disconnect the UI.
- Bare `devflow` and no-arg `devflow tui` are launch/reconnect entry points, not passive status viewers. They must ensure the default target is running in daemon watch mode and must not treat a stale empty status snapshot as ready. `devflow tui --instance <id>` is the explicit attach-only path.
- Daemon start is intentionally idempotent for "ensure this target is already running" paths, but operator relaunch actions such as invalidate/rerun must force-stop matching active work first. Otherwise the idempotence check can turn a relaunch request into a no-op.
- The old hidden `__internal_exec` and `__internal_supervise` launcher routes are removed from the CLI path. Keep only compatibility cleanup for persisted supervisor/executor PIDs and their process trees from older state.
- User-facing adapters should use the builder/component API rather than hand-assembling low-level `project.Task` slices. The low-level structs remain the engine representation, but docs and new examples should teach `project.Define`, `project.Builder`, `database.Postgres`, `database.Prisma`, and `database.PayloadCMS`.
- Finite builder tasks become cacheable when they declare project output paths. Do not ask adapter authors to call `Cache()` for normal generated artifacts. Use `Stamp()` for local install/setup commands such as `npm install` that should run once per input key without caching heavyweight mutable folders. Stamped install decisions are per-worktree and must not consult or restore from the global task cache. Use `NoCache()` for finite actions that should execute every time they are scheduled, such as tests or migration authoring.
- Keep task cache storage in one OS user cache folder (`<os.UserCacheDir()>/devflow/cache`) and namespace entries by project. Per-worktree logs/state stay in the worktree `.devflow/`.
- Prefer narrow, semantic fingerprints over hashing the whole repo. Use `Inputs.Filtered`/`project.Filtered(...)` when the normal graph inputs are right but only part of each file should affect a cache key. Keep this generic: filters live in `pkg/project`, watch matching remains path-based, and helpers such as `GoCommentLinesStartingWith("@")` plus `GoStructDeclarations()` should support Swagger-like cases without hardcoding Swaggo into core. Filtered-content hash caching is engine-owned and in-memory only; do not persist it to `.devflow/` or the global task cache unless profiling later proves that is necessary.
- Custom fingerprint functions are evaluated and their returned values enter the task key; never serialize callback function values as part of the normalized task signature. Task-signature normalization must operate on cloned slices so fingerprint calculation cannot reorder adapter task definitions.
- Optimize cache storage only after correctness and contract coverage exist.
- Watch reruns must preserve graph dependency barriers. If an intermediate task is blocked from running in watch mode, downstream tasks in that cascade must not run against stale intermediate outputs.
- Idle watch daemons should not poll the whole worktree when the selected target closure has declared file inputs. Watch scoping should stay based on task inputs plus flush sync sentinels, with heavyweight dependency folders such as `node_modules` ignored by default unless explicitly declared. Repeated writes to an already-pending file, especially flush sync sentinel retouches, must not extend the debounce window forever.
- Watch tests that edit files after startup must wait for the engine's `watch.ready` marker, not just for initial task counters or service state. The watcher is started after the initial graph run, so a fast Linux runner can otherwise write before the polling baseline exists and miss the first edit.
- Watch tests should assert selective behavior from settled baselines: affected services may restart more than once on some platforms during a cascade, while unaffected tasks/services should remain at their baseline count. Avoid exact absolute rerun counts for affected long-lived services unless the engine contract truly guarantees them.
- Watch service restart policies should stay explicit: default to affected-slice restarts, use `RestartNever` to block watch restarts, and reserve `RestartAlways` for services that must bounce on every target-affecting watch cycle.
- Treat `devflow flush --json` as the AI readiness gate for detached watch/dev workflows: edit files, flush the selected target closure, then run tests only when flush reports success.
- Go debugging is a first-class debug-service concept (`KindDebugService` through `b.GoDebugService` or raw-task `project.GoDebugService`), not a hidden flag on normal services. The implemented Delve path is external `go build -gcflags=all=-N -l` into a stable worktree-local debug binary, then `dlv exec --headless --api-version=2 --accept-multiclient --continue` on a stable localhost debug port. Devflow, through the per-worktree daemon, owns stop/rebuild/relaunch on watch changes and exposes debug attach metadata through `NodeStatus.Debug`. Avoid attach-to-existing-process, hand-written adapter-local Delve runners, and editor-owned restart flows in round one; they do not fit Devflow's restart-on-change ownership.
- CI installs Delve `v1.26.3` (including the macOS DWARFv5 fix) and runs real `dlv exec` smoke tests on Linux, macOS, and Windows, including a watch-mode source edit that must relaunch the debug service with a new PID, live debug listener, and intact attach metadata. On macOS, a locally installed `dlv` is not sufficient if `DevToolsSecurity -status` is disabled; do not treat the resulting debugserver launch failure as a Devflow regression or change machine security settings automatically.
- Windows cleanup remains the main risk for Delve support. Current process cleanup uses process groups on Unix and `taskkill /T /F` on Windows; a future Job Object path may harden Windows further. Keep fake lifecycle tests and later real Delve tests focused on proving Delve/debuggee trees do not survive restarts or stop paths.

## Current Shape

Devflow is now beyond the initial bootstrap. The core includes graph validation, fingerprinting, snapshot caching, process supervision, instance and port state, bounded parallel engine scheduling, typed events, a per-worktree daemon, polling watch mode, flush readiness coordination, required CLI checks/installers, interactive prompt plumbing, a TUI, first-class Go debug services, and Docker-backed Postgres runtime helpers.

Runtime adapters are project-local:
- installed `devflow` or the repo-level source launcher builds/uses the bootstrap binary
- a selected worktree must contain `devflow.project.go`
- the bootstrap CLI compiles `<worktree>/.devflow/bin/devflow-local`
- project-local binary stale checks and rebuilds are serialized by `<worktree>/.devflow/localbuild.lock`
- the repo-local launcher rebuilds its source binary from a content build key rather than file mtimes
- normal commands exec into that worktree-local binary
- there is no built-in adapter fallback when `devflow.project.go` is missing

The first real BikeCoach integration showed that adoption hardening is now more important than expanding operator features in the abstract. After localbuild locking, reliable detached cleanup, explicit service lifecycle contracts, graph affected explanations, target-scoped required CLI checks, and managed Postgres host-port hardening, the highest-value issues are complete user examples for script convergence, fixed ports, and managed Postgres.

Service lifecycle contract: bare `devflow` is the day-to-day operator entry and should ensure the default target is running in daemon-owned watch mode before opening the TUI. Non-CI `run`, `watch`, `flush`, `restart`, `stop`, and TUI actions are daemon-backed through one daemon per worktree. Attached non-CI `run` is foreground from the client perspective and blocks while daemon-owned services live. `run --ci` bypasses the daemon, may start services as readiness probes, and stops them before returning. `run --detach` only proves daemon launch of the target; `watch --detach` plus `flush` is the readiness-gated background workflow for humans and agents.

Flush startup contract: a newly detached watcher can write `watch.ready` before its first polling scan has fully settled, so `flush` must keep rewriting the sync sentinel while waiting for the ack. Do not remove that retry unless the watcher startup protocol is made stronger.

Watch/input debugging contract: `Inputs.Ignore` is shared by fingerprinting and watch matching. Patterns are slash-normalized, checked root-relative, and for directory inputs also checked relative to the input dir. Builder `Inputs("path")` maps to path inputs, `project.Glob("...")` maps to glob inputs with `**` support, and `project.Filtered(...)` maps to a watched input path whose cache key hashes filtered bytes only. Watch polling is scoped from those same target-closure inputs, so missing inputs can mean both missed invalidation and missed file pickup. `devflow graph affected --files <path> --explain --json` is the first tool to use when generated files cause surprising watch cascades.

Required CLI contract: `RequiredCLIs()` is the project catalog. `RequiredCLIs` on tasks and targets selects the subset needed for a target closure. `devflow doctor --target <target> --json` and `devflow clis status/install --target <target>` must not report unrelated catalog entries. The older `Dependencies()` provider remains only as a compatibility path.

Managed Postgres contract: app code connects through the host-mapped port, so database readiness must include host-port readiness, not only in-container `pg_isready`. `EnsureRuntime` preserves volumes but recreates stale containers whose published host/container port mapping, resolved image, named volume, or volume destination no longer matches the current instance. Persist database flavor, PostGIS PostgreSQL major, custom container-port, and snapshot-sidecar configuration in `api.DBInstance`; do not accept those config values and then silently use constants. Defaults are the multi-architecture official `postgres:16.14` and `alpine:3.24.1` images with no forced Docker platform, so Apple Silicon runs native `linux/arm64`. `database.PostGIS(name, major)` requires 16, 17, or 18. It uses the matching maintained `postgis/postgis` tag on Docker amd64 and builds the embedded `postgres:<major>-bookworm` plus matching Debian PostGIS-package recipe to a major- and recipe-versioned local image on Docker arm64; use Docker engine architecture, not Go host architecture, for that decision, and require extension activation as part of readiness. PostGIS volume names include the major to prevent accidental physical-cluster reuse across upgrades. PostgreSQL 18 mounts `/var/lib/postgresql`, unlike the `/var/lib/postgresql/data` destination required by 16/17. Check/pull or build a missing image with the long data timeout before issuing the short-timeout detached container start; cold-machine downloads/builds must not be hidden inside a 15-second control operation. Managed database operations use the official Docker Engine Go API with bounded contexts and no `docker` command subprocess or fallback. Endpoint precedence is `DOCKER_HOST`, `DOCKER_CONTEXT`, Docker config `currentContext`, then the OS default; preserve Docker's Unix-socket, Windows named-pipe, TCP/TLS, and SSH context behavior. Keep the SDK behind the package-owned `dockerEngine` boundary. Long-lived database tasks use `StartRuntimeService`, register its PID-less `project.ServiceHandle`, and forward logs through `Runtime.LineEmitter`; lower-level SQL adapters use `Manager.ExecSQL` rather than `docker exec`. Never add adapter-owned `docker info`, `docker logs`, `docker exec`, or wrapper-shell shutdown paths. Generic handles, not OS PIDs alone, define service liveness, so PID `0` is valid for Engine-managed resources and must still work in readiness, flush, watch restart, CI cleanup, and attached shutdown. Snapshot/restore must stream tar data through the container archive API and gzip locally rather than bind-mounting host snapshot paths, which is a key Windows/Docker Desktop portability invariant. `devflow stop --all` stops the managed DB container recorded on the instance while preserving the Docker volume. Migration snapshots must be prefix-safe and resolved-image/platform-compatible. `ApplyEach` preserves exhaustive per-prefix snapshots; the default Prisma workflow treats committed migration history as stable, snapshots intermediate prefixes only around migration folders with uncommitted Git changes, falls back to final-only snapshotting when Git cannot identify those folders, and always snapshots the final state.

Physical DB snapshot portability contract: snapshot keys are single directory names and must be rejected before Docker or destructive filesystem work when they are absolute, nested, or parent-relative. Manifest version 3 records the resolved Docker image, platform, and configured PostgreSQL major. Managed migration restore must treat missing required platform/version metadata, another architecture, another PostgreSQL major, or another resolved image/flavor as a cache miss before destroying runtime state, then rebuild from source/migrations. Direct restore returns `ErrSnapshotIncompatible`. This is especially important for Intel-to-Apple-Silicon moves, Postgres-to-PostGIS changes, and custom image aliases reused across majors; `.devflow/db-snapshots` is a local acceleration cache, not a cross-machine backup format.

Prisma workflow contract: normal DB preparation applies migrations with `prisma migrate deploy` and snapshots useful migration-prefix milestones; migration authoring is separate. Useful default milestones are based on Git commit state, not migration count: committed history normally applies as one stable tail, while migration directories with uncommitted Git changes get prefix boundaries for edit/retry ergonomics. Prisma migration inspection ignores non-directory entries such as `migration_lock.toml`. If `schema.prisma` declares models but no migrations exist, or changes without a new migration, the default Prisma workflow should return an explicit migration-needed error. The engine records errors implementing `MigrationNeeded() bool`, plus known Prisma migration-needed messages, as `migration_needed`, not generic `failed`, and downstream work must remain pending until the migration is authored. `prisma.NewMigration(b)` must reconcile the managed DB through the authoring-prep path before invoking Prisma migration generation, so edited latest migrations restore the previous prefix and reapply the changed tail instead of hitting Prisma's modified-applied migration reset prompt.

Project actions are first-class explicit foreground operations, separate from normal DAG targets. Migration creation is the first standard action kind, `devflow.database.migration.create`; `devflow action run ...`, `devflow migration create ...`, and the TUI `m` key should discover and invoke actions rather than infer target names. Normal `up`, `watch`, `test`, and `flush` apply/check existing migration artifacts but must not create migrations implicitly. Component helpers such as `prisma.NewMigration(b)` and `payload.NewMigration(b)` register task-backed actions; do not teach or preserve `new-migration`/`migration_new` target conventions.
TUI database/migration contract: the TUI may expose managed database identity and cached Prisma migration-prefix snapshot metadata for operator visibility. Its explicit `m` action asks for a migration name, sends a daemon action with kind `devflow.database.migration.create`, streams daemon task/log progress to the footer status immediately, surfaces declared interactive prompts, and relaunches the previously detached target after success so services restart through the graph. It must not hand-run Prisma/Payload on a detached side path or turn normal boot/watch flows into implicit migration authoring or reset flows. The common component task names are `prisma_client`, `prisma_migrations`, `prisma_new_migration`, `payload_migrations`, and `payload_new_migration`.

PayloadCMS/Postgres contract: normal `up`/watch targets apply existing Payload migrations through `payload.Migrations(b)` and should not create migrations implicitly. Payload collection/global modules must be part of the migration task inputs so watch sees schema edits; the component defaults to `src/collections` and `src/globals`, and adapters can override with `SchemaInputs(...)`. Migration creation uses the action registered by `payload.NewMigration(b)`, reads the action input `name` through `DEVFLOW_MIGRATION_NAME`, and can surface Payload confirmation prompts for destructive changes through the generic interactive prompt path. Prompt specs support alternate `Patterns` and `Repeat` for tools that ask multiple similar confirmation questions.

Remote clone contract: `PostgresDumpSourcePolicy` invokes host `pg_dump` and `psql` directly, without a Unix shell or unchecked pipeline. A failed dump, including host/client version mismatch, must fail the Devflow task before restore begins. Components using `CloneFromEnv` declare both clients in target-scoped required-CLI checks; on Apple Silicon Homebrew's keg-only `libpq` bin directory may need to be added to `PATH`.

Runtime env and secrets: instance env is persisted under `.devflow/state`. Adapters should avoid storing long-lived production secrets there, avoid logging whole env maps, and override runtime values such as `PORT` for unit-test tasks when those tests should not inherit service runtime ports.

State is split deliberately:
- per-worktree logs and instance snapshots live under the worktree `.devflow/`
- all projects share one physical task cache under the OS user cache dir, with entries namespaced by project
- daemon sockets live under a short per-user temp path such as `/tmp/devflow-daemon-<uid>/` to avoid Unix socket path length failures in nested worktrees
- sibling git worktrees share port allocation through the Git common dir
- non-git temp/test flows fall back to local/global safe defaults
- `status --json` may retain the desired managed DB identity after `stop --all`; container liveness is not implied by the presence of `db` metadata

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
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
```

Useful command smoke checks:

```bash
go run ./cmd/devflow doctor --json
go run ./cmd/devflow status --json
go run ./cmd/devflow run <target> --json --project <name>
```

The full Docker-backed database integration suite is opt-in locally and should be run when changing `pkg/database` runtime behavior; the remote Postgres clone and focused PostGIS spatial cases also run in GitHub Actions on native Linux amd64 and Linux arm64. GitHub-hosted macOS arm64 cannot provide Docker Desktop because nested virtualization is unavailable, so keep the normal macOS test matrix for deterministic planning coverage and run the Docker case locally/self-hosted on Apple Silicon:

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
- Keep the full real Docker e2e suite opt-in locally, but retain the remote Postgres clone and focused PostGIS spatial tests as required GitHub Actions coverage on native Linux amd64 and Linux arm64. Do not put Docker jobs on GitHub-hosted macOS runners; they lack the nested virtualization required for Docker Desktop.
