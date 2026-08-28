# Architecture

## Core Layers

`cmd/devflow` is thin CLI wiring over the packages in `pkg/`.

- `pkg/project`: task, target, runtime, adapter interfaces, and generic tasklets such as output-converging finite commands
- `pkg/graph`: validation, topo ordering, closures, and affected-task calculation
- `pkg/fingerprint`: deterministic file, directory, env, and task-key hashing
- `pkg/cache`: manifest, snapshot, restore, and cache lookup
- `pkg/process`: one-shot execution, supervised services, line-buffered logs
- `pkg/project`: also defines readiness hooks for service tasks
- `pkg/database`: Docker-backed dedicated Postgres runtime and snapshot helpers
- `pkg/instance`: worktree-scoped instance identity and persisted state
- `pkg/ports`: shared port registry with lock-safe allocation
- `pkg/engine`: bounded parallel ready-queue execution engine and status persistence
- `pkg/daemon`: per-worktree daemon, JSON-line socket protocol, action queue, and event fanout for mutable dev/watch/operator work
- `pkg/event`: typed event bus used by the engine for run, task-state, cache, process, instance, and log events
- `pkg/watch`: Devflow-owned polling file scanner and debounced change batching
- `pkg/validation`: finite sandbox execution for input/output contract checks and exhaustive dependency-valid task-order checks

## Local Project Bootstrap

Runtime project configuration is now project-local.

Flow:

- the installed `devflow` binary, or the repo-level `devflow` launcher during source development, looks for `./devflow.project.go` in the selected project worktree
- if the file is missing, the command fails
- if the file exists, the bootstrap CLI discovers it first plus lexically sorted, regular root-level `devflow_*.go` companions; `devflow_*_test.go`, unrelated Go files, nested files, and non-regular matches are excluded or rejected as appropriate
- the bootstrap CLI compiles that ordered source set into a worktree-local full CLI binary
- stale checks and rebuilds are guarded by a per-worktree lock at `<worktree>/.devflow/localbuild.lock`; commands that waited for another builder re-check the build key before compiling
- execution is then transferred into that compiled local binary for all normal commands

`localProjectSourceFiles` owns the adapter discovery contract. Every adapter filename/source label and its contents participate in the content key along with the existing Devflow version/bootstrap inputs. Discovery happens again after acquiring the localbuild lock, and that exact post-lock ordered adapter set is copied into the reconstructed generated module. This preserves add/remove/rename invalidation, stable timestamp-only reuse, serialized builds, and removal of stale companions.

The worktree does not have to live under the Devflow source checkout. Source-local bootstrap hashes repo sources relative to the checkout when possible, and hashes external project files with stable external labels so Windows temp worktrees on another drive still build.

On Unix platforms, transfer into the worktree-local binary uses process replacement. Windows does not support `syscall.Exec`, so the bootstrap process runs the worktree-local binary as a child and exits with the child's status.

Current local binary location:
- `<worktree>/.devflow/bin/devflow-local`

Current generated build location:
- `<worktree>/.devflow/localbuild/<worktree-hash>/`

Current localbuild lock location:
- `<worktree>/.devflow/localbuild.lock`

The generated build is a small Go module whose path is under `github.com/benjaco/devflow/localbuild/...`, which allows it to import Devflow's `internal/cli` package. Installed binaries require the released Devflow module version. Source-local development uses:

```go
replace github.com/benjaco/devflow => <devflow-source-root>
```

in that generated module, preserving fast local iteration without requiring a released tag for every source change.

Project-source constraint:

- `devflow.project.go` remains the required marker and normally registers the project in `init()`
- it and every optional root-level `devflow_*.go` companion compile together as `package main`
- `_test.go` companions remain normal project tests and are not copied into the runtime adapter module
- arbitrary sibling Go files and recursive adapter directories are deliberately outside the loader contract

This model intentionally avoids:
- built-in runtime adapter registries
- runtime JSON adapter protocols
- dynamic plugin loading tricks

## State Layout

Per-worktree state lives under `.devflow/`:
- `.devflow/logs/<instance-id>/`
- `.devflow/state/instances/<instance-id>/`
- `.devflow/state/instances/<instance-id>/flush/`
- `.devflow/state/instances/<instance-id>/payload-schema/` for password-free Payload schema-push fingerprints

Daemon/supervisor state is also per worktree. The instance snapshot records:
- the per-worktree daemon PID as the supervisor PID
- legacy child executor PIDs when old supervisor state is being reconciled
- service task PIDs when the supervised service is an operating-system process; managed resources such as database containers have no host PID entry
- the daemon log path

The engine, not the CLI or persisted PID registry, owns live service handles. Each daemon active run therefore carries an `engine.LifecycleController`. Task-scoped stop/restart commands are serialized through the engine loop, which verifies a monotonic service generation and readiness before returning. Late `Wait` results from a replaced handle are ignored by generation. Watch mode monitors every current generation after readiness, so an unexpected Delve/service exit becomes terminal status without tearing down independent services. Direct OS process termination remains only the recovery path when no live owning engine exists; `stop --all` remains the complete cleanup boundary.

Task cache storage is global for the user:
- `<os.UserCacheDir()>/devflow/cache`

Entries are namespaced inside that physical cache root:
- `entries/<project-cache-namespace>/<task>/<fingerprint-key>/`

Projects can implement `CacheNamespace() string`; otherwise the project name is used. This keeps one cache folder on the system while avoiding accidental collisions between project adapters.

Repo-shared coordination state for sibling git worktrees still lives under the Git common dir from:
- `git rev-parse --git-common-dir`

Current shared paths:
- `<git-common-dir>/devflow/state/ports.json`

Global coordination state that is not repo-specific still lives under the user cache directory:
- `devflow/state/instance-index.json`

The daemon Unix socket lives in a short per-user temp directory such as `/tmp/devflow-daemon-<uid>/<instance-id>.sock`. It is intentionally not stored under deeply nested worktree paths because Unix socket path length limits are easy to hit on macOS.

This split keeps runtime logs and instance state local to the worktree, keeps task cache globally reusable, keeps port allocation coordinated for sibling git worktrees, and keeps socket paths short enough for real terminals and test worktrees.

Structured state files and `runtime.env` are replaced through unique temporary files in the same directory so a failed or concurrent write cannot expose a partially truncated destination. Same-destination replacements are serialized within a process. On Windows, the final `MoveFileEx(..., REPLACE_EXISTING)` operation and JSON readers both retry bounded transient access, sharing, and lock violations because concurrent daemon/engine operations and file scanners can briefly hold the destination. The existing destination is never removed first, so readers still see either the old complete file or the new complete file. On Unix-like systems these persisted files are owner-readable/writable only (`0600`), because instance JSON and runtime env can contain local database credentials or other sensitive development values. This is local hardening, not encryption.

Task, daemon, and event-stream log files are also created and repaired to owner-only `0600` permissions on Unix-like systems. They can contain command output derived from runtime configuration and must not default to group/world-readable files.

Flush coordination is per instance:
- `flush/requests/<request-id>.json` records the requested sync point
- `flush/sync/<request-id>.sync` is the file-watcher sentinel
- `flush/acks/<request-id>.json` stores the final `FlushResult`
- `flush/watch.ready` is an internal readiness marker written after detached watch startup has reached the file-watching phase

## Runtime Env

Instance env is now explicit and layered:
- persisted values from the prior instance state as a recovery baseline
- optional `.env` values and adapter defaults from `ConfigureInstance`
- values from the invoking process for env keys explicitly used by the project
- devflow-managed instance overrides such as IDs, ports, and database connection values

The important rule is precedence:
- dotenv/adapter values are defaults
- an explicitly set CI or shell value wins for declared task input env, required env, or another project-configured env key
- devflow-managed runtime values win last

Devflow deliberately selects only project-relevant process variables instead of persisting the entire caller environment. That allows projects to keep normal local app settings in `.env`, lets CI override those defaults, and still ensures launched processes point at the correct per-instance Postgres runtime and leased ports.

Instance env is persisted under `.devflow/state` so detached supervisors, status, and relaunches can recover the same runtime configuration. Do not treat it as encrypted secret storage. Adapters should avoid storing long-lived production secrets there, avoid logging full env maps, and override runtime values such as `PORT` for unit-test tasks when those tests should not inherit the service runtime port.

## Service Supervision Boundary

The engine supervises `project.ServiceHandle`, not only child processes. A handle reports liveness, waits for termination, stops idempotently, and may expose a host PID. `process.Handle` implements that contract for command-backed services. Concurrent calls to its `Stop` method join the same bounded terminate/kill operation; a context watcher cannot make engine cleanup return before the child has been reaped. Engine-managed resources can return PID `0`; the engine retains their in-memory handle for readiness, flush health, watch restarts, CI cleanup, and attached-run shutdown without persisting a false OS-process reference.

Database adapters use `database.Manager.StartRuntimeService` for this path. It ensures the container, follows stdout/stderr through the Docker Engine log API, waits for container termination through the Engine API, and stops the container through the Engine API. Adapters register the returned handle with `Runtime.RegisterServiceHandle` and route its log callback through `Runtime.LineEmitter`. A wrapper process running `docker logs -f` is neither required nor permitted by the managed-database portability contract.

## Watch Cascades

Watch mode uses task inputs as the file-change interface:
- `Inputs.Paths` matches exact relative paths and descendants
- `Inputs.Files` matches exact relative files
- `Inputs.Dirs` matches relative directories and descendants
- `Inputs.Globs` matches slash-normalized glob patterns, including `**`
- `Inputs.Ignore` can suppress matching paths

When a file batch arrives, the engine:
- finds directly affected tasks in the selected target closure
- expands through the downstream task graph using watch restart policy rules
- prunes tasks that cannot run in watch mode, such as warmups without `AllowInWatch` and services with `RestartNever`
- adds services marked `RestartAlways` so they restart on any watch cycle that affects the selected target
- preserves dependency barriers while pruning

The dependency-barrier rule is important: if an intermediate candidate is blocked from the watch cycle, its downstream candidates are blocked too. Downstream tasks must not run in advance against stale intermediate outputs just because they are also reachable from the changed task.

Normal ready-queue scheduling still applies to the final rerun set, so included downstream tasks become runnable only after included upstream dependencies finish or restore from cache.

Ignore semantics are shared by watch matching and fingerprinting:
- paths are slash-normalized before matching
- glob-style matches use exact path matching
- a non-glob directory pattern also suppresses descendants by path prefix
- for directory inputs, ignore patterns are checked both root-relative and relative to the input directory
- for explicit file inputs, root-relative ignore patterns can suppress that file from both watch matching and fingerprinting

This lets adapters use either `internal/storage/sqlc` or `sqlc` to ignore generated files under `Inputs.Dirs: []string{"internal/storage"}`. `devflow graph affected --files <path> --explain --json` exposes which input matched or which ignore pattern suppressed a file.

New user-facing adapters should normally use the builder API, where `Inputs("path")` populates `Inputs.Paths` and `project.Glob("internal/storage/**/*.sql")` populates `Inputs.Globs`. The older `Files`/`Dirs` fields remain the lower-level internal representation for existing engine tests and helpers.

Current service restart policy meanings in watch mode:
- `RestartNever`: never restart from file-change cascades
- `RestartOnInputChange`: restart only when the service is in the affected downstream slice
- `RestartAlways`: restart on every watch cycle that has at least one directly affected task in the selected target

## Flush Readiness Gate

`devflow flush [target]` coordinates with detached watch mode through the per-instance flush files. The command writes a request file and then writes the sync sentinel under `.devflow/state/instances/<instance-id>/flush/sync/`. While waiting for the ack, the CLI periodically rewrites the sync sentinel. This makes the first flush after `watch --detach` resilient to watcher startup races where the file watcher has written `watch.ready` but has not completed its first polling scan yet.

The watch runner normally ignores `.devflow`, but the engine explicitly includes the flush sync directory in its watcher inputs. When a batch arrives, the engine splits flush sync files out of normal changed files:
- normal user file changes run through the existing watch cascade logic first
- sync files are not treated as task inputs
- after the cycle completes, the engine loads each matching request and writes an ack
- sync-only batches still produce an ack after health evaluation

This proves that edits completed before the `flush` command wrote the sentinel have crossed the watcher's file-change boundary before the command returns success.

Flush health is scoped to the selected target closure:
- once, group, and warmup tasks must be `done` or `cached`
- service tasks must be `running`
- the registered service handle must still be alive; process-backed handles additionally require a live host PID
- service readiness hooks must pass when defined
- services outside the selected target closure are not part of flush success

Version 1 reports unhealthy in-chain services as `service_unhealthy` issues. It does not auto-restart unhealthy services during `flush`.

## Pipeline Validation

`devflow validate <target>` is a direct, finite verification surface. It does not use the worktree daemon, task cache, or task stamps, and tasks receive `Runtime.Mode == api.ModeValidation` together with `DEVFLOW_VALIDATION=1` and the selected `DEVFLOW_VALIDATION_MODE`.

Artifact validation uses one disposable worktree and a deterministic dependency order. Before each finite task, Devflow resets that sandbox and materializes only:

- the task's declared worktree file/path/dir/glob/filtered inputs, with normal ignore rules
- declared outputs archived from the task's transitive dependencies

The task then runs normally. Devflow compares filesystem snapshots around the task, reports final file changes outside its declared output paths, verifies every declared output exists with the requested file/directory type, and archives only declared outputs for downstream tasks. This proves that the explicit worktree inputs plus dependency outputs are sufficient for the observed run. It cannot prove that every declared input is necessary, observe a temporary file created and removed entirely during the task, or trace resources outside the sandbox.

Order validation enumerates every topological ordering of the selected target closure. Each ordering starts from the same reset copy of the source worktree with `.git`, `.devflow`, and non-in-place declared outputs removed. Tasks execute one at a time with cache/stamp bypass. Every order must finish successfully, produce all declared outputs, and yield the same final declared-output content/type/mode snapshot as the first successful order. This catches undeclared dependency edges as well as order-dependent artifact generation.

Exhaustiveness is a contract, not a best-effort label. The command enumerates up to `--max-orders` (default 1000) before running permutations. If the graph has more valid orders, it runs none of them, returns `complete=false`, and asks the caller to raise the bound. It never reports a sampled prefix as successful exhaustive validation.

Validation only accepts target closures without service or debug-service tasks because those tasks do not finish one by one. `.git` and `.devflow` are reserved sandbox paths, absolute/escaping declarations and worktree-root outputs are rejected, and overlapping outputs from different tasks are rejected as ambiguous ownership. The shared projection copier preserves relative symlinks whose fully resolved targets remain inside the selected source projection and rejects absolute, escaping, or externally resolving symlinks; it never expands a pnpm-style internal link graph by dereferencing it. Directories remain writable while children are copied and receive their source permissions only after the subtree is complete. Copy operations honor cancellation, report progress, and cleanup first repairs restrictive copied permissions without following symlinks.

Artifact validation formerly copied each dependency's declared outputs from the active sandbox into a separate archive tree. That made the projection and archive two simultaneously expanded copies. The artifact lifecycle now transfers those outputs into validation-owned holding directories with same-filesystem renames, checks them back out before consumers run, and transfers the next outputs back after execution. This preserves isolation without writable hardlinks, avoids a second full materialization on filesystems with or without reflinks, and bypasses the normal persistent task cache. A single validation-wide budget counts every projection, snapshot, and output-transfer phase instead of resetting per copier call. It records cumulative files/logical bytes, current and peak validation-specific logical bytes, measured current/peak allocated filesystem bytes on Linux/macOS, phase timing, and remaining limits; Windows reports the physical-measurement flag as false while retaining exact logical metrics. Before materialization it checks available space against a safety reserve; limit failures return structured phase/resource/observed/limit data and cleanup makes restrictive partial trees removable. Task-defined effects outside `Runtime.Worktree`—databases, networks, absolute paths, global tool caches, and processes not registered as services—are not isolated, so users should validate finite, side-effect-safe targets.

## Interactive Commands

Devflow should treat subprocess interactivity as an exception, not the default execution model.

Policy:
- normal `run`, `watch`, and boot targets should be non-interactive
- adapters should prefer explicit non-interactive flags such as `-y`, `--yes`, `--force`, or `CI=1` where that is safe
- if a task would require a destructive or ambiguous choice, the adapter should model that as an explicit action or separate target instead of letting the process block on stdin

This keeps DAG execution deterministic and prevents background runs, detached supervisors, and watch mode from hanging on hidden prompts.

### Prisma-Specific Rules

Prisma needs special handling because its CLI mixes normal migration application with authoring and reset flows.

Rules:
- normal startup flows should not depend on interactive `prisma migrate dev`
- normal DB prep should restore a snapshot, then apply the known remaining migrations non-interactively
- creating a new migration should be a separate explicit operator action because it requires a provided migration name
- destructive reset should be a separate explicit operator action and should not happen implicitly during boot

Recommended command usage:
- create a named migration:
  - `prisma migrate dev --name <name>`
- create the migration without applying it yet:
  - `prisma migrate dev --name <name> --create-only`
- reset only when the user has explicitly chosen reset:
  - `prisma migrate reset --force`

Important limitation:
- from Devflow's perspective, `prisma migrate dev` is still not fully deterministic in drift/reset scenarios, because Prisma may still require an operator decision

Design implication:
- migration authoring and reset flows belong in explicit commands, TUI actions, or future interactive task support
- normal boot/watch targets should stay on the snapshot-plus-replay path instead of relying on Prisma prompts

### Implemented Interactive Prompt Path

Devflow now supports prompt-driven interactive one-shot commands without blocking invisibly in detached mode.

Current behavior:
- tasks can mark a subprocess command as interactive through `process.CommandSpec`
- the command declares expected prompt patterns and prompt kinds
- prompt specs can provide alternate `Patterns` and `Repeat` for tools that emit different or repeated confirmations, such as destructive migration warnings
- when a prompt pattern is detected in subprocess output, the engine emits an `interaction_requested` event
- the engine waits for an answer file under the instance state directory
- when an answer arrives, the engine writes it back to the subprocess stdin and emits `interaction_answered`

The current transport is file-backed:
- request metadata is carried on the event stream
- answers are written to `.devflow/state/instances/<instance-id>/interactions/<prompt-id>.json`

This is enough for detached runs and the TUI to cooperate without shared in-memory state.

Current limitation:
- this is prompt-pattern and stdin based, not full TTY emulation
- commands that require a true terminal rather than prompt/answer stdin handling still need a future PTY-specific path

## Required CLI Installation

Adapters define required command-line tools together with platform-specific install scripts.

`RequiredCLIs()` is the project-level catalog at the engine boundary. New adapters normally populate it through `project.Builder.RequiredCLIs` or `RequiredCLI`. Builder command tasks automatically select a matching catalog entry when the command name matches it. Tasks and targets select from that catalog with `RequiredCLIs`, allowing target-scoped commands to avoid over-reporting tools that belong only to unrelated flows. The older `Dependencies()` provider remains as a compatibility shim for early adapters.

Current shape:

```go
type RequiredCLI struct {
    Name        string
    Command     string
    Description string
    Install     map[string]InstallScript
}

type Task struct {
    Deps         []string
    RequiredCLIs []string
}

type Target struct {
    RootTasks    []string
    RequiredCLIs []string
	RequiredEnv  []string
}
```

Semantics:
- required CLI status is determined by checking whether the command is available on `PATH`
- `devflow doctor` checks the full project required CLI catalog for backward compatibility
- `devflow doctor --target <target>` checks only CLIs required by the target and its task closure
- `devflow clis status/install --target <target>` use the same scoped selection
- `RequiredCLIs` entries may reference either required CLI `Name` or `Command`
- `clis install` only runs installers for commands that are currently missing
- after an installer runs, Devflow re-checks that the command now resolves
- install scripts are selected by platform (`darwin`, `linux`, `windows`, or `unix`)
- `Builder.RequiredEnv(...)`, `TaskBuilder.RequiredEnv(...)`, and target `RequiredEnv` metadata declare values that must be non-empty
- target-scoped doctor checks only required env from the target/task closure, plus project-wide requirements
- `doctor --strict` prints the normal text or JSON result and exits nonzero when a CLI or env check fails

This keeps required CLI policy adapter-defined while giving the core CLI a stable install surface for humans, CI, and agents.

## Database Isolation

The chosen direction is now full per-worktree separation for local databases:
- one Postgres container per worktree instance
- one dedicated host port per worktree instance
- one dedicated Docker volume per worktree instance

The new `pkg/database` package provides the runtime primitives for that model:
- derive deterministic per-instance container and volume names
- ensure the container is running, recreating stale containers whose published host/container port mapping or configured image no longer matches the selected instance while preserving the volume
- wait for readiness via `pg_isready`, host-port readiness when the DB instance has a host, and a successful container-loopback TCP `psql` probe against the exact configured database; forcing TCP avoids mistaking the official image's socket-only bootstrap server for the final runtime
- stop or destroy the runtime
- snapshot and restore the Postgres data volume with the sidecar image persisted in instance/manifest state
- inspect Prisma schema/migration state and choose the nearest cached migration-prefix snapshot

The default runtime images are `postgres:16.14` and `alpine:3.24.1`. Both are official multi-architecture images, and no platform override is added, so Docker selects native `linux/arm64` images on Apple Silicon and native `linux/amd64` images on x86 hosts. Low-level `Config.ContainerPort` and `Config.SidecarImage` values must survive into persisted `api.DBInstance` state; snapshot/restore and readiness must use those persisted values instead of silently falling back to package constants.

Managed database operations use the official Docker Engine Go client, not `docker` command subprocesses. Endpoint resolution follows Docker precedence in-process: `DOCKER_HOST`, `DOCKER_CONTEXT`, the Docker config's `currentContext`, then the platform default. Docker's context transport supplies Unix sockets, Windows named pipes, TCP/TLS, and SSH context support. The native Windows default is `npipe:////./pipe/docker_engine`; macOS and Linux use the selected context or Unix socket. The Docker executable is therefore not a managed-database prerequisite, although a reachable Engine (normally Docker Desktop on macOS/Windows) still is.

Long-lived database service supervision uses the same client. `StartRuntimeService` follows multiplexed container stdout/stderr and waits for container exit through Engine API streams, returning a PID-less `project.ServiceHandle` that the engine can stop and health-check. After a normal exit, the follower drains the Engine log response to EOF so buffered final frames are not lost; cancellation and Engine wait errors close the stream to unblock it. `Manager.ExecSQL` provides structured in-container `psql` execution for lower-level migration adapters. Together they remove adapter-owned `docker info`, `docker logs`, `docker exec`, and wrapper-shell shutdown paths while preserving task logs and typed log events.

`database.PostGIS(name, postgresVersion)` is a persisted database flavor and PostgreSQL-major contract, not an adapter-specific image override. Supported majors are 16, 17, and 18. Runtime image resolution uses Docker engine architecture because that is the platform which executes the container. On amd64/x86_64 it resolves to `postgis/postgis:<major>-<postgis>` (`3.5` for PostgreSQL 16/17 and `3.6` for 18). On arm64/aarch64 it resolves to `devflow/postgis:<major>-bookworm-postgis3-arm64-v1`, built from `postgres:<major>-bookworm` with matching package names through the Dockerfile embedded from `pkg/database/docker/postgis-arm64.Dockerfile`. The generated image tag includes both the PostgreSQL major and recipe revision so version/recipe changes reconcile stale containers without aliasing the image cache. Explicit `Config.Image`/component `Image(...)` overrides architecture selection but still requires a matching supported major. Readiness includes idempotent `CREATE EXTENSION IF NOT EXISTS postgis` in the configured database.

PostGIS volume identity includes `-pg<major>` so changing the parameter cannot attach an older physical cluster to a newer PostgreSQL server. Versions 16 and 17 mount their named volume at `/var/lib/postgresql/data`; version 18 mounts at `/var/lib/postgresql`, following the official image's breaking `PGDATA`/`VOLUME` layout change. Existing containers are reconciled when the named volume or destination differs. This is isolation, not a major-version migration facility; use logical dumps or an explicit `pg_upgrade` flow to carry data forward.

Volume snapshots are physical Postgres cluster archives. Snapshot and restore stream tar data through the Engine container archive API and gzip locally; they do not expose host paths to a sidecar bind mount. This removes host-path syntax and sharing differences between Unix, Docker Desktop, and Windows. Snapshot keys must be one directory name rather than an absolute, nested, or parent-relative path; validate them before any Docker call or filesystem removal. Snapshot manifest version 3 records the resolved Docker image, its OS/architecture, and the PostgreSQL major when configured. Managed Prisma/migration restore treats legacy manifests without required platform/version metadata, manifests whose platform or PostgreSQL major differs, and manifests produced by another resolved image as cache misses before any container or volume is destroyed. This protects Intel-to-ARM moves, Postgres-to-PostGIS flavor changes, and custom-image aliases reused across PostgreSQL majors. Direct `RestoreSnapshot` returns `ErrSnapshotIncompatible` for the same conditions. Do not turn `.devflow/db-snapshots` into a cross-machine data transfer format; use logical dumps for that purpose.

Engine control-plane calls such as inspect, create/start, stop, and remove are bounded by short context deadlines. Cold image acquisition is resolved explicitly with image inspection plus a separately bounded long Engine pull; do not let a first-machine image download happen implicitly inside the short container-start deadline. Image builds and streaming snapshot/restore work use the longer data deadline. Context cancellation closes attached exec streams, so an unavailable or stuck Engine surfaces as a database task error instead of leaving a task in `running` forever.

This keeps DB isolation strong and avoids shared-cluster coupling between worktrees.

What this package does not decide:
- when to clone remote state versus run a bootstrap script
- which schema fingerprint should own the snapshot key

Those decisions belong in adapter policy layered on top of the runtime module. The package now provides the snapshot-planning primitives plus a source-policy hook for snapshot misses; the adapter still needs to decide which base source to use, when to snapshot, and which inputs define the base fingerprint.

### DB Source Policies

Snapshot misses should rebuild from a configured base source, not from an implicit reset action.

Current shape:

```go
type SourcePolicy interface {
    Name() string
    PrepareBase(ctx context.Context, db api.DBInstance, opts PrepareOptions) error
}
```

Behavior:
- first try an exact or nearest-prefix snapshot restore
- if that fails, destroy the current runtime/volume
- if a source policy is configured:
  - start a temporary local Postgres runtime
  - apply the source policy
  - stop the runtime
- then continue with normal migration replay and snapshotting

This matches the intended operator model:
- reuse the latest compatible local volume when possible
- otherwise rebuild from a configured base source such as:
  - a remote dev clone script
  - a local bootstrap/startup script later
- never "skip" a changed migration in the middle; restore falls back only by valid prefix

The bundled example adapter now exercises this shape structurally:
- inspect Prisma state
- restore the nearest snapshot or recreate from the configured base source
- start a temporary DB runtime
- replay remaining migrations
- snapshot the prepared state
- start the final per-instance Postgres service for app runtime

Higher-level workflow helpers now exist on top of those primitives:
- `database.Postgres` for the common managed local Postgres instance wiring, including default snapshot root, runtime env, and port allocation
- `database.Prisma` for common Prisma tasks: client generation, migration-prefix DB preparation, and explicit migration authoring
- `database.PayloadCMS` for common PayloadCMS migration application and explicit migration authoring against managed Postgres, including prompt specs for confirmation-heavy migration creation
- `EnsureMigratedDatabase` for generic migration folders
- `PostgresMigrationFileApplier` for applying one SQL file per migration and snapshotting every prefix
- `EnsurePrismaDevDatabase` for Prisma schema + migration folders, applying pending migrations through prefix-limited `prisma migrate deploy` runs by default
- `PreparePrismaMigrationAuthoringDatabase` for reconciling a managed Prisma database before migration authoring
- `GeneratePrismaMigration` for explicit Prisma migration authoring
- `PostgresDumpSourcePolicy` for cloning a remote development Postgres database into the local runtime

Prisma migration inspection is directory-only. Files under `prisma/migrations`, including `migration_lock.toml`, are not migration points and must not affect prefix counts or snapshot keys.

The important cache invariant is prefix safety. A snapshot can only be reused when its migration list is a valid prefix of the current migration list and the base fingerprint still matches. `EnsureMigratedDatabase` with `ApplyEach` snapshots every prefix after applying it. The default `EnsurePrismaDevDatabase` path is less chatty: committed migration history is treated as stable and applies as one tail before the final snapshot. Intermediate Prisma snapshots are created only at boundaries needed for migration folders with uncommitted Git changes, plus the final state. If Git is unavailable or the worktree is not a Git repository, the default falls back to final-only snapshotting. That preserves local migration editing without running Prisma once per historical committed migration on cold rebuilds. Adapters that need exhaustive Prisma prefix snapshots can still provide `MigrateEach`.

Prisma has two authoring guards: if `schema.prisma` declares models but no migrations exist, or if `schema.prisma` changes but the migration list has not advanced beyond the restored prefix, the default workflow returns a migration-needed error telling the adapter to generate a migration first. The engine writes errors that implement `MigrationNeeded() bool`, plus known Prisma migration-needed messages, as `migration_needed` rather than `failed`, and downstream work remains pending. Migration generation must be modeled as an explicit action using `PreparePrismaMigrationAuthoringDatabase` plus `GeneratePrismaMigration`, or as the explicit TUI `m` action, not hidden inside normal `up`.

Prisma database preparation emits progress lines before snapshot planning, runtime recreation, readiness waits, source-policy application, and final runtime start. This is intentionally visible in task logs and the TUI footer because Docker reconciliation can happen before any Prisma subprocess writes output. Progress helpers own writing those component progress lines to the task log and event stream; subprocess output is written to the task log by `pkg/process` and forwarded to live consumers through an event-only callback so Prisma CLI output is not duplicated.

Migration authoring prep intentionally differs from normal DB prep: it restores/rebuilds the managed database to the best compatible prefix, reapplies any missing or edited tail migrations, and does not snapshot the schema-drift state it prepares for Prisma. That lets `prisma migrate dev --create-only` compare the current schema against a compatible database without hitting Prisma's "migration was modified after it was applied" reset prompt after a developer edits the latest migration.

Adapters may override Prisma migration execution with `Migrate` or `MigrateEach`. `Migrate` is an all-at-once command and only snapshots the final state; `MigrateEach` preserves the exhaustive per-prefix cache contract.

`PostgresDumpSourcePolicy` must fail when `pg_dump` fails. In its default host-client strategy it writes through an owner-only temporary dump file instead of an unchecked shell pipeline so `psql` cannot mask a failed clone with an empty successful restore. It invokes `pg_dump` and `psql` as separate commands without a Unix shell. Passwords are supplied through a temporary owner-only `PGPASSFILE`; command arguments and inherited PostgreSQL URL variables are sanitized, and `PGPASSWORD` is cleared. PostgreSQL `password` query parameters are percent-decoded into the pgpass secret and removed from every reconstructed URL; when both userinfo and query credentials exist, the query value wins, matching libpq. All repeated `password` parameters are removed while ordinary connection options remain. Credential-bearing query channels that Devflow cannot transport safely (`sslpassword`, `oauth_client_secret`, and `passfile`) are rejected with credential-redacted errors.

The opt-in `PostgresClientContainer` strategy runs the clients already bundled in the managed Postgres image. `PrismaComponent.CloneFromEnvContainerized(...)` selects it and removes host `pg_dump`/`psql` requirements, making a reachable Docker Engine sufficient for most remote-clone workflows. The Docker exec command and env contain only password-free URLs; the two pgpass records are sent through exec stdin into owner-only temporary files that are trapped for cleanup. A remote URL using host-local `localhost` is not automatically reachable from inside the managed container and must use a container-reachable hostname. The client major follows the selected managed image and must be compatible with the remote server; adapters that need another client major should select a compatible managed image or retain the host-client strategy.

PayloadCMS follows the same operator rule as Prisma: normal `up`/watch paths apply existing migrations non-interactively through `payload.Migrations(b)`, while migration creation belongs to an explicit action registered by `payload.NewMigration(b)`. Payload can ask for confirmations when changes may be destructive; those prompts flow through the generic interactive prompt path instead of being handled with Payload-specific TUI logic. Payload schema module paths are part of the component input contract, not only app-service inputs: by default the component includes `src/collections`, `src/globals`, and `src/fields`, and adapters can override or extend them with `SchemaInputs(...)`/`AddSchemaInputs(...)`. Migration authoring uses `project.CommandOutputTasklet` with new-file semantics over the configured migration directory, because Payload can return zero without writing a migration. It retries only successful missing-output attempts, preserves non-zero failures, and never cleans the migration directory.

`PayloadCMSComponent.ConfigureDevService` is the development schema-push boundary. It adds config/schema/package-lock inputs directly to the service so watch mode selects a restart, computes a narrow content fingerprint plus a normalized password-free Postgres identity in `BeforeRun`, and supplies `PAYLOAD_SCHEMA_PUSH` only to that task's cloned runtime env. A pending fingerprint is not authoritative. `AfterReady` atomically promotes it to applied state only after explicit service readiness succeeds; readiness failure stops the service and leaves the prior applied key untouched. The Payload config consumes the flag with `push: process.env.PAYLOAD_SCHEMA_PUSH === 'true'`. This keeps the core hooks generic while Payload-specific paths, state, and environment behavior remain in `pkg/database`.

`project.CommandOutputTasklet` is the generic finite-command convergence boundary. Required paths/globs are worktree-relative and every pattern must match a regular file. It can require matches created during the current run, rechecks after a bounded settle delay before rerunning, and honors context cancellation. Optional cleanup is restricted to explicit worktree-relative output directories that contain required patterns and exact `OutputHashFiles`. Hash cleanup cannot run independently: every directory and hash path is validated before mutation, then hashes are removed before output directories in the same one-time pre-attempt phase so a directory-removal failure cannot leave stale validity state. Cleanup rejects escaping, globbed, Git/Devflow-state, non-file hash, or symlink-traversing paths. Graph output declarations remain owned by `project.Task`; the tasklet does not infer cache artifacts.

Managed Postgres target pattern:
- preserve the Docker volume unless an explicit restore/rebuild path owns the destruction
- call `EnsureRuntime` before migration/app tasks
- call `WaitReady` before connecting through the host DSN; it checks Docker readiness, the host-mapped port, and a real TCP query against the exact configured database
- run migrations against `db.URL`, not a container-local address
- stop the final DB container through `devflow stop --all`; this preserves the Docker volume

Do not unconditionally remove the DB container in normal startup. Docker port mappings are immutable, so `EnsureRuntime` removes and recreates only stale containers with a wrong published port while preserving the volume.

## Cache Keys

The default cache key is derived automatically from:
- engine key version
- task name
- normalized task signature
- dependency result keys
- selected file and directory hashes
- selected filtered file-content hashes
- selected env values
- custom fingerprint outputs

`devflow cache key --target <target> --json` evaluates the same task-key path without executing tasks and returns an aggregate target key plus each cacheable/stamped task key. It resolves normal instance env, ports, and finalizers so keys match a real run. `devflow cache path --json` exposes the OS cache root and selected namespace path for CI cache integrations without requiring callers to reconstruct platform-specific paths.

For CI systems that must restore an external cache before running, `cache key --manifest-out <path>` writes an owner-only schema-v1 manifest and `run --cache-key-manifest <path> --ci` reuses its captured custom/semantic fingerprint digests. The manifest binds the 15-minute snapshot to the Devflow build, project/cache namespace, worktree instance, target, graph and task signatures, instance configuration, environment-value hashes, local-input digest, semantic component digests, and preflight task keys. It contains hashes rather than plaintext environment or fingerprint values. Loading is strict and size-bounded, verifies the integrity checksum and every binding, and rejects corrupt, incompatible, expired, wrong-target, or changed-environment manifests without falling back to callback execution. The checksum detects accidental/modifying handoff corruption; it is not an authenticity signature against a writer who can replace the owner-only file.

Execution never blindly trusts the preflight final task key. It rehashes cheap local inputs and rebuilds dependency keys, so a changed source file or newly generated upstream output changes the affected final key while the deliberately captured remote semantic component remains fixed for the handoff snapshot. Run results expose manifest validation duration, reused tasks/components, and local-input changes; cache timing records which components came from the manifest.

Custom fingerprint callbacks are executable adapter behavior, so the function values themselves are never JSON-serialized into the normalized task signature. The engine evaluates each callback and includes its returned value in the task key. Adapter authors should keep those values deterministic and use the task's explicit `Signature` when changing task behavior that cannot be represented by declared inputs. Signature normalization works on cloned slices so calculating a cache key cannot reorder the adapter's task definition in memory.

Filtered inputs are generic `project.Inputs.Filtered` entries. The task declares a file, directory, or glob plus a signed content filter. Fingerprinting reads each matching file, runs the filter, and hashes only the filtered bytes; files with empty filtered output do not contribute an input hash. The filter signature is part of the normalized task signature, so changing the filter invalidates prior keys.

Engines own an in-memory filtered-content hash cache for this path. Cache entries are keyed by absolute file path, file size, file modtime, and filter signature. This avoids reparsing unchanged files during daemon/watch/TUI loops, especially for AST-based filters, while keeping changed files re-filtered before deciding whether the task key changed. The cache is deliberately not persisted to `.devflow/` or the global task cache.

The built-in helper filters live in `pkg/project` rather than in any framework package:
- `LinesStartingWith(...)`
- `GoCommentLinesStartingWith(...)`
- `GoStructDeclarations()`, including leading doc comments attached to each struct
- `CombineContentFilters(...)`

Watch matching remains path-based. A filtered input still contributes its declared file, directory, or glob base to the selected target's watch paths, and `graph affected --explain` reports `filtered` or `filtered_glob` when a file change matches it. The expensive task command may then be skipped through the unchanged filtered cache key. This keeps the core generic while supporting semantic cases such as Swagger comments plus Go structs.

## Local Task Stamps

Stamped tasks are finite `KindOnce` tasks that use the normal task key but do not write a global cache entry. On success, the engine writes a tiny per-worktree stamp under:

```text
<worktree>/.devflow/state/instances/<instance-id>/task-stamps/
```

On a later run in the same worktree, a matching stamp marks the task done without executing it, provided any declared local outputs still exist. This supports install/setup commands such as `npm install`: the task reruns when `package.json` or `package-lock.json` changes, but Devflow does not copy `node_modules` into `<os.UserCacheDir()>/devflow/cache` or consult the global cache to decide whether the install is already valid. If `node_modules` is declared as an output and is deleted in that worktree, the stamp is ignored and the install task runs again.

Stamped tasks are invalidated together with cacheable tasks in daemon/TUI invalidation flows, but they are not cache hits and should not be reported as restored artifacts.

### Task-Defined Cache Key Override

Some tasks can compute a better semantic cache identity than the generic automatic key. The design therefore allows a cacheable one-shot task to define its own cache-key function.

Current task-model shape:

```go
type CacheKeyFunc func(ctx context.Context, rt *Runtime) (string, error)

type Task struct {
    // ...
    CacheKeyOverride CacheKeyFunc
}
```

Semantics:
- `CacheKeyOverride` is optional.
- It applies only to cacheable `KindOnce` tasks.
- When present, it replaces the automatic key body for that task.
- The engine should still namespace the final key with at least:
  - engine key version
  - task name
  - the override result
- The engine should not silently mix automatic inputs into an override key. If a task chooses override mode, that override is authoritative.

Recommended use cases:
- backend artifact fingerprints
- DB-state fingerprints
- adapter-specific semantic versions
- externally known content digests

Guideline:
- use the automatic key unless the adapter can define a narrower and more correct semantic key
- when using an override, the task author is responsible for including any dependency/version/config data needed for correctness

## Built Binary Helpers

`pkg/project` now includes a generic helper for software-build tools that need to be compiled once and reused by later tasks.

Current shape:

```go
type BinaryTool struct {
    TaskName    string
    Description string
    Deps        []string
    Inputs      Inputs
    Output      string
    Build       process.CommandSpec
    Signature   string
    Tags        []string
}
```

Semantics:
- `BuildTask()` returns a cacheable `KindOnce` task
- the task key still comes from the normal task-input fingerprint model
- the built artifact is cached as a declared output file
- later tasks can call `tool.Run(...)` or `tool.Start(...)` to execute the built artifact

This is intended for helper binaries such as code generators, schema tools, or repo-local build utilities. The helper stays generic: the engine does not know how the binary is produced, only which inputs fingerprint it and which output file should be cached.

## Go Debug Services

Go debugging is a distinct service shape rather than a normal service with a `dlv` command pasted into `Command(...)`.

The implemented round-one model:
- adapters declare `b.GoDebugService("<task>")`
- the task kind is `debug_service`, but the engine treats it as service-like for scheduling, watch restart, flush health, and stop cleanup
- build a debug binary explicitly with `go build -gcflags=all=-N -l`
- write it to a stable worktree-local path such as `.devflow/debug/<service>`
- start Delve with `dlv exec <binary> --headless --api-version=2 --listen=127.0.0.1:<debug-port> --accept-multiclient --continue -- <app-args>`
- allocate the debug endpoint as a stable named localhost port
- expose editor attach metadata through `NodeStatus.Debug` in `status --json`
- require debugger readiness by probing the debug TCP port
- compose app readiness through the existing service readiness model when the adapter calls `ReadyHTTP`, `ReadyTCP`, `ReadyFile`, or `Ready`
- on watch changes, stop the old supervised Delve process tree, rebuild, and relaunch on the same named port

The builder API is `b.GoDebugService(...)`. Raw-task adapters should use `project.GoDebugService(...)` and `project.GoDebugServiceOptions` so the Delve lifecycle remains centralized in `pkg/project` instead of being copied into example or project adapter files.

This is different from normal service supervision because Delve owns a launched debuggee process and editor attachment is stateful. Devflow should still be the outer owner through the per-worktree daemon. Round one should avoid attach-to-existing-process workflows and editor-driven Delve restart orchestration.

Cross-platform cleanup is part of the architecture, not a test afterthought. Unix starts supervised processes in a process group and escalates from graceful signal to process-group kill. Windows currently uses process-tree termination through `taskkill /T /F`. Future hardening can replace that with Job Objects, but debug service tests must keep proving that watch restart and stop paths do not leave orphaned Delve/debuggee processes or locked debug binaries.

## Event Stream

The engine now emits a typed in-process event stream for live consumers. Event categories include:
- run started / finished
- watch cycle started / finished
- instance updated
- task state changed
- cache hit / miss
- log line
- process exited
- interaction requested / answered / cancelled

The daemon subscribes to engine events, persists them, and fans them out over its JSON-line socket to live consumers such as the TUI. Those observer subscriptions remain best-effort so a slow UI cannot block execution. Direct `run --ci --json` instead registers a lossless, backpressured in-process subscription before execution starts: burst task state, cache, and log progress cannot be dropped from stderr, while stdout remains a single final JSON result suitable for parsers.

For watch cycles specifically:
- `files` now carries the raw changed worktree-relative file paths from the watcher batch
- `affectedTasks` carries the directly affected task names derived from those file changes

Daemon-owned runs also persist the engine event stream to:
- `.devflow/state/instances/<instance-id>/events.jsonl`

The TUI uses the daemon socket subscription as its primary live-update signal and the persisted event stream as a fallback/recovery source.

## Service Readiness

Service tasks can now declare adapter-defined readiness checks.

Current task-model shape:

```go
type ReadyFunc func(ctx context.Context, rt *Runtime) error

type Task struct {
    // ...
    BeforeRun    RunFunc
    Ready        ReadyFunc
    AfterReady   RunFunc
    ReadyTimeout time.Duration
}
```

Semantics:
- readiness is optional and applies to service tasks
- `BeforeRun` receives a task-local clone of runtime env and may prepare per-start values without changing persisted instance env
- the process is started first
- the task is only marked `running` after readiness passes
- `AfterReady`, when defined, runs after readiness and before the task is marked running; it requires an explicit readiness function
- if readiness fails, times out, or the process exits first, the task becomes `failed`
- a failed readiness or `AfterReady` attempt stops the service process before returning

The current helper surface includes:
- `ReadyAll(...)`
- `ReadyFile(...)`
- `ReadyPath(...)`
- `ReadyTCPPort(...)`
- `ReadyHTTPNamedPort(...)`

Default behavior:
- tasks without `Ready` are considered ready immediately after process start
- tasks with `Ready` use a default timeout when `ReadyTimeout` is unset

This keeps the core generic while letting adapters define the right readiness signal for each service.

## Service Lifecycle Contract

Service tasks have different command semantics depending on the run mode:
- attached non-CI `run` is a foreground operator command backed by the per-worktree daemon. It starts services, waits for readiness, and then blocks while those services run. A service exit ends the attached run; an external stop may surface as a service-exited error.
- `run --ci` is deliberately direct and finite, not daemon-backed. Services are allowed as readiness probes: Devflow starts them, waits for readiness, stops them, clears persisted service PIDs, and records the service nodes as `stopped` before returning success.
- `run --detach` asks the per-worktree daemon to start the target in the background and returns after launch. It does not prove the target closure is healthy.
- `watch --detach` asks the daemon to start the development loop. It is the expected long-running mode for humans and agents that want automatic reruns after file edits.
- `flush` is the daemon-backed watch readiness gate. It proves the watcher observed the post-edit sync sentinel, waits for the selected target closure to settle, and checks service health.
- `stop --all` asks the daemon to stop active work. It reconciles legacy supervisors, child executors and their process trees, tracked services, stale status PIDs, and the instance-managed database container. Stopping the database container preserves its Docker volume. After sending the response, the daemon shuts itself down so stopped state is not reported as a live daemon.

The current automation recommendation is intentionally explicit: use detached watch plus `flush` for "background environment is ready" workflows. Do not reinterpret attached `run` as a start-and-return command without adding a separate CLI contract.

Finite check/test targets with service dependencies should generally use `run --ci`, because plain attached `run` keeps service dependencies alive.

## Watch Mode

Watch mode uses a polling watcher with debounced batches. The engine scopes the watcher to the selected target closure's declared file inputs plus the flush sync directory. It does not intentionally poll the whole worktree when the closure has concrete `Inputs(...)`, `Files`, `Dirs`, or `Globs`; common heavyweight folders such as `node_modules` are ignored by default unless explicitly watched by an input path.

On each batch:
- changed files are mapped to task inputs
- the affected downstream slice inside the target closure is computed
- impacted running services are stopped first
- affected one-shot tasks rerun in dependency order with normal cache semantics
- impacted services restart after their dependencies complete and are only considered back once readiness passes

Watch propagation now treats service-to-service dependency edges specially:
- service restarts do not automatically cascade into downstream services by default
- a downstream service must opt into that behavior with `WatchRestartOnServiceDeps`

This prevents backend-service bounces from needlessly forcing frontend-service restarts, while still allowing explicit infrastructure-style dependencies such as `postgres -> backend`.

The watch engine still runs in-process relative to its owner, but for user-facing dev/watch/operator workflows that owner is the per-worktree daemon. This keeps one mutable source of truth for service lifecycle, status, file watching, and event streaming.

## Operator Controls

The current operator surface now includes:
- PID-based `stop` for tracked service tasks
- daemon-backed background launch for service-bearing runs
- cache inspection and invalidation
- cache garbage collection
- limited non-service `restart`
- detached service `restart` by restarting the last daemon target

Mutable ownership is implemented by one daemon per worktree. CLI and TUI operations connect to that daemon over a JSON-line Unix socket. The daemon owns:
- active engine run/watch context
- service process handles
- status writes
- event persistence and live event fanout
- flush sync/ack coordination
- TUI actions such as invalidate/rerun, retarget, and Prisma migration authoring

Daemon startup is serialized by a per-instance file lock under worktree state so concurrent CLI/TUI commands cannot start competing daemons for the same worktree.

TUI daemon ownership is explicit. If bare `devflow` or `devflow tui` creates the daemon for that TUI session, TUI exit sends the normal all-work stop request so active services, managed databases, and the daemon shut down together. If the TUI connects to a daemon that already existed, TUI exit only disconnects the UI.

The older hidden `__internal_exec` and `__internal_supervise` launcher paths are no longer user-facing execution routes. Their persisted supervisor/executor state and process-tree descendants can still be reconciled during `stop --all` so existing stale processes are not orphaned.

The daemon persists:
- daemon PID as the supervisor PID
- daemon log path
- last run config
- legacy supervisor/executor PIDs as process refs when replacing old state

This is enough for:
- `run --detach`
- `watch --detach`
- `stop --all` against daemon-owned work; it snapshots live resources before engine cancellation clears references, terminates legacy supervisor/executor process groups and descendants, tracked service process groups and PID-bearing status nodes, stops the managed database only when its container is running, and reports only confirmed actual stops before shutting the daemon down
- service `restart` by asking the daemon to relaunch the last active target

The operator surface now also reconciles detached state when queried:
- `status` uses the daemon when one is already running, otherwise reads persisted state without starting a new daemon; it includes daemon PID/liveness plus sanitized instance metadata such as ports, URLs, and DB identity when present
- `logs supervisor` reads the daemon/supervisor log directly

The first usable TUI slice is now implemented as a local terminal console connected to the per-worktree daemon, with persisted state as fallback. It currently provides:
- live daemon event subscription plus fallback persisted-event refresh
- task selection
- selected-task details
- task log tail
- daemon/supervisor log toggle
- database/Prisma panel showing managed Postgres identity and recent Prisma migration-prefix snapshots
- explicit migration generation from inside the TUI by asking for a migration name, sending a daemon migration-create action through the daemon-owned engine, surfacing declared prompts, and relaunching the previously detached target after success
- instance/worktree/runtime header
- stable terminal rendering via a real TUI library instead of manual ANSI frame painting
- one-key invalidate-and-rerun from the selected task, without a TUI confirmation modal, by sending a daemon action that invalidates the selected downstream cacheable once-task slice and relaunches the current target
- prompt popups for interactive confirm and text questions emitted by daemon-owned work
- lifecycle overlays own their input until dismissal: Escape is consumed before global quit handling, Enter executes a confirmed plan once, and closing an overlay restores the prior dashboard pane without changing task selection or viewport
- retargeting uses a stable, vertically scrollable target list that marks the active target and shows the complete selected name before opening the existing impact preview
- the middle workspace is responsive: terminals at least 120 columns wide and 24 rows high place the stable task selector on the left and logs on the right, while compact terminals retain the vertical task-over-log layout
- log following is explicit per-source user state keyed only by durable instance/source/task identity; snapshots never change PAUSED/FOLLOWING, and paused views retain an absolute logical-line anchor that is translated into each retained range after append, pagination, truncation, rotation, or temporarily missing log metadata

The tview before-draw hook may resize or draw primitives, but must not call application methods such as `GetFocus`, `SetFocus`, or page visibility helpers that reacquire tview's application lock. DevFlow tracks task/log focus from focus callbacks and input handling; too-small rendering is performed directly on the supplied screen. This preserves focus styling without making the first frame re-enter a non-reentrant application mutex.
