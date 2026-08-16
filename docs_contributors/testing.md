# Testing

Devflow uses three testing layers:

GitHub Actions treats Go 1.26.6 as the supported minimum. Push and pull-request validation runs `go test ./...` plus `go build ./cmd/devflow` on exactly Go 1.26.6 across Linux amd64, macOS 15 arm64, and Windows amd64, and also retains a rolling current-stable compatibility lane on Linux. Each matrix entry asserts its native Go host architecture so the macOS job remains a real Apple Silicon gate rather than silently becoming an Intel/emulated check. The matrix installs Delve with `go install github.com/go-delve/delve/cmd/dlv@v1.26.3`; this patch line includes the macOS DWARFv5 fix and supports Go 1.26. Real Go debug-service smoke coverage runs on each OS. A separate Linux job runs `go test -race ./...`. A Docker database job installs PostgreSQL clients, asserts the Docker engine architecture, verifies PID-less managed-container log/watch/stop supervision plus Engine-API SQL execution, runs the real remote schema/data clone across distinct non-default ports, and exercises PostGIS spatial queries plus persistence for PostgreSQL 16, 17, and 18 on native Ubuntu amd64 and Ubuntu arm64 runners. GitHub-hosted macOS arm64 runners do not support the nested virtualization needed for Docker Desktop, so the macOS matrix runs all deterministic PostGIS selection/build-planning tests while real Docker-on-Mac coverage remains a local/self-hosted check.

The Linux quality job enforces `gofmt`, clean `go mod tidy -diff` output, `go vet`, Staticcheck `v0.7.0`, and govulncheck `v1.6.0`. Dependency and GitHub Actions update checks are scheduled monthly through Dependabot. Tag verification repeats the Go 1.26.6 test/build matrix on Linux, macOS, and Windows.

Cross-platform tests should avoid Unix-only assumptions unless the test is guarded by build tags or an explicit platform skip. Prefer generated Go helper binaries over shell-script fake tools, add `.exe` to built helper paths on Windows, and use Go encoders for JSON fixtures so Windows paths are escaped correctly. Long-running process tests should verify process-tree cleanup on Windows because orphaned children can keep task log files locked after the parent exits.

Real Delve app-readiness tests on macOS require the normal Delve prerequisites, including enabled Developer Tools security. Check `DevToolsSecurity -status`; if it is disabled, follow Delve's installation guidance before interpreting `stub exited while waiting for connection` as a Devflow regression. Do not change machine security settings automatically from tests.

Tests that assert exact cache hit/miss or watch-rerun counts must isolate the OS user cache root. Set `HOME`, `XDG_CACHE_HOME`, and `LOCALAPPDATA`; Windows uses `LOCALAPPDATA` for `os.UserCacheDir()`, so `HOME` alone is not enough.

## Unit Tests

- graph validation and closures
- exhaustive topological-order enumeration with explicit combinatorial bounds
- fingerprint determinism, including custom callback fingerprints that remain executable rather than being JSON-serialized and task-signature normalization that does not mutate adapter task definitions
- filtered file-content fingerprints, including semantic no-op edits, Go `@` comments, Go struct declarations with doc comments, in-memory filtered-hash reuse for unchanged files, and engine-level cache behavior where irrelevant edits restore cache while relevant filtered edits rerun the task
- cache snapshot and restore semantics, including read-only directory modes, preserved pnpm-style internal relative symlinks, external-link rejection, bounded entry/byte limits, progress callbacks, and writable cleanup
- cache-key override stability and correctness
- instance identity and env persistence
- port allocation and reuse
- atomic JSON/runtime-env replacement under repeated concurrent writes, failed-marshalling preservation, owner-only Unix permissions, bounded Windows destination-sharing retry, cross-platform file-lock serialization, and non-blocking concurrent event fanout

## Integration Tests

- subprocess stdout/stderr capture, 256 KiB output lines, propagated scanner errors for lines above the bounded 4 MiB maximum, and pipe draining so oversized lines cannot stall child exit
- per-task log truncation at task-attempt start so custom adapter progress and subprocess output both reflect the current run attempt, with owner-only Unix log permissions
- interactive prompt detection and answer forwarding with a real prompt CLI fixture, including alternate/repeated prompt patterns
- service lifecycle management for both process-backed and PID-less managed-resource handles, including CI shutdown and flush health
- CI-mode service targets act as readiness probes and stop services before returning
- Go debug-service coverage includes builder API metadata, external debug binary build planning, status attach metadata, stable debug-port readiness, watch restart sequencing, and process-tree cleanup through fake `go`/`dlv` helper binaries. CI also installs real Delve and runs real `dlv exec` coverage on Linux, macOS, and Windows for both CI-mode readiness/stop and watch-mode source-edit restart back into a debug service. The real watch restart test starts an HTTP app under Delve, verifies the first response body, edits the source constant, waits for the restart, and verifies the endpoint returns the new body. Real editor attach coverage remains opt-in until the debug-service contract is proven in real projects.
- daemon lifecycle coverage for per-worktree daemon startup locking, CLI connection, daemon event persistence/fanout, daemon executable refresh after source/local binary changes, daemon log path creation, and preserving legacy supervisor/executor PIDs for later cleanup
- stop cleanup for daemon-owned work, daemon shutdown after `stop --all`, legacy supervisor/executor refs and descendants, tracked service, and stale status process groups
- read-only `status` coverage proving stopped-state inspection does not start a daemon
- `stop --all` cleanup for the instance-managed database container while preserving the volume
- service readiness success and timeout behavior
- built-binary helper build/run/start coverage and cache-restore coverage
- database runtime command planning and snapshot-manifest coverage, including cold-image pull ordering, configured image/volume reconciliation, custom container ports, custom snapshot sidecars, escaped DSNs, PostgreSQL 16/17/18 PostGIS amd64 pull/arm64 build selection, version-specific volume destinations, extension readiness, unsupported-version rejection, legacy default fallback, and pre-destructive cache misses for legacy/cross-architecture/cross-image physical snapshots
- builder API coverage for command tasks, services, target definitions, automatic cacheability from outputs, port env references, dotenv loading, path inputs, glob inputs, and filtered inputs
- output-converging command tasklet coverage for zero-exit/no-file retries, exact and glob requirements, new-file semantics, delayed writes, bounded exhaustion, cancellation, immediate command failures, one-time output-plus-hash cleanup, cleanup ordering/prevalidation, and traversal/symlink rejection
- local stamped task coverage for install/setup commands that skip on matching input keys, rerun when declared local outputs are missing, stay isolated per worktree, ignore matching global cache entries, and avoid watch loops when commands touch unchanged input mtimes
- database component coverage for common Postgres/Prisma/PayloadCMS task generation, optional snapshot-root defaults, instance DB/env finalization, explicit non-cacheable migration authoring, and containerized remote-clone selection without host libpq CLI requirements
- database runtime host-port and exact-database SQL readiness, stale published-port reconciliation, bounded Engine API calls, exec stdin/env support and cancellation, deterministic parsing of exec-start JSON before stdin on the hijacked stream, stdout/stderr log demultiplexing, normal-exit log-stream draining, structured container-create requests, Docker-context precedence, Windows named-pipe client construction, and managed-container service shutdown
- managed migration workflow coverage for exact snapshot reuse, nearest-prefix restore, incompatible base fingerprint misses, changed-latest tail replay, per-migration prefix snapshots, Prisma default milestone snapshots based on uncommitted migration folders, direct Git-status parsing for modified/untracked Prisma migration folders, final-only snapshotting for committed Prisma tails, checked remote Postgres clone policy failures, owner-only `PGPASSFILE` credentials with sanitized process metadata, userinfo/query-password precedence and percent-decoding, ambient PostgreSQL URL/`PGPASSWORD`/`PGPASSFILE` sanitization, rejected unsupported credential query channels, containerized-client stdin-only credential transport, Prisma directory-only migration inspection, Prisma lock-file churn, model-free schemas without migrations, added/deleted Prisma migrations, changed older Prisma migration rebuilds, failed Prisma migration apply without snapshotting, Prisma prefix deploy snapshots, fresh/no-migration migration-needed errors, schema-without-migration migration-needed errors, Prisma authoring database reconciliation, database prep progress logs before Docker/Prisma subprocess output, no duplicated runtime subprocess log lines, and Prisma generate helpers
- Prisma schema/migration inspection and nearest-prefix snapshot planning coverage
- PayloadCMS/Postgres example coverage for project detection, graph shape, migration apply command wiring, watch pickup for collection/global module edits, deleted-field schema edits that create a migration only after a confirmation prompt, and retry after a fake Payload/npm zero exit that writes no migration
- dotenv parsing and merged runtime-env coverage proving declared invoking-process values override defaults while devflow-managed ports/database URLs win last
- CLI JSON output shape, including command-level lifecycle coverage for `run`, lossless burst CI stderr progress with stdout-only final failure diagnostics, bounded early-marker excerpts, Go test/compiler classification, non-overlapping windows, and trigger retention under aggregate caps, `status`, `logs`, `instances`, target-scoped required-env doctor/strict exits, cache key/path/manifest handoff, and `stop`
- validation sandbox coverage for declared-input sufficiency, transitive dependency-output transfer without a second expanded copy, read-only Go-module-like trees, large preserved pnpm symlink layouts, external symlink rejection, undeclared/missing outputs, bounded summary/issues JSON with exact counts and exhaustive full mode, stderr phase/counter progress, shared multi-phase budget exhaustion, disk reserve reporting, cancellation and writable cleanup, source/cache immutability, output-owner collisions, service rejection, every dependency-valid sequential order, missing dependency edges, cross-order artifact mismatches, order-limit refusal, real-worktree non-mutation, and stable stdout-only final JSON; the compiled-CLI bootstrap test must also load a project-local adapter and prove both artifact and missing-order-dependency findings end to end
- scoped docs command coverage for `devflow docs setup`, `devflow docs development`, and the bare `devflow docs` usage error
- CLI help coverage for important `run` flags such as `--ci`, `--detach`, and `--watch`
- Go-first release command coverage for `version` and `upgrade`, with `upgrade` tested through a delayed fake `go` executable on `PATH` to prove child output is observable before completion and JSON stdout stays machine-clean
- installed/source bootstrap coverage for generated local modules, source `replace` directives, single- and multi-file project adapters, deterministic companion discovery/copying, add/edit/remove/rename build-key invalidation, timestamp/test/unrelated-file exclusions, rejected non-regular matches, failed-build binary preservation, external-worktree/Windows paths, and portable concurrent localbuild serialization
- global cache coverage for the single OS user cache root and project cache namespaces
- required CLI detection, target-scoped required CLI selection, project/task required-env selection, doctor source reporting/strict failure, and platform-script install coverage in `pkg/project` and `internal/cli`
- engine-level interactive prompt event plus answer-file integration coverage
- TUI database/Prisma panel rendering, drift warning, Prisma snapshot-summary loading, daemon-backed migration-create action, detached-target relaunch, selected-task invalidate/rerun forced relaunch behavior, log scroll preservation across same-log reloads, default startup waiting for a matching non-empty status snapshot instead of stale blank state, and progress/footer status coverage
- TUI daemon ownership coverage proving a daemon created for the TUI session is stopped on exit while an already-running daemon is left alone
- sequential engine execution with cache hits/misses, per-node duration/cache timings including completed zero-tick measurements on coarse platform clocks, bounded failure log tails/excerpts, planned target-cache keys that match execution keys, and cache-key manifest creation/reuse/rejection including local/generated-input changes and one total semantic-callback invocation
- distinct canceled-vs-failed task-state behavior when sibling task failure cancels in-flight work, plus explicit `migration_needed` task-state classification for database migration authoring guards
- scheduler error preservation so a canceled sibling does not replace the first actionable task failure with `context canceled`
- polling watch batching, declared-input watch scoping including filtered-input glob bases, default `node_modules` ignore behavior, repeated flush-sentinel retouch debounce behavior, and selective watch reruns. Tests that edit files after startup should wait for the engine `watch.ready` marker before writing; initial task counters alone do not prove the polling watcher baseline has started.
- graph affected explanations for path, file, directory, glob, filtered, ignored, and unmatched paths
- watch cascade pruning so downstream tasks do not run past warmups or services that are blocked from watch execution, including full watch execution and mixed blocked/allowed branch coverage
- watch service restart policies, including `RestartAlways` selection and full watch execution behavior
- flush coordination coverage for request/ack path generation, watcher inclusion of the flush sync directory under `.devflow`, engine ack timing after reruns and sync-only batches, failed-task ack issues, service readiness health issues, CLI daemon/timeout behavior, structured low-level daemon failures, older-daemon error preservation, and sync-sentinel retouch while waiting for an ack
- opt-in real Docker-backed database runtime snapshot/restore coverage in `pkg/database`
- opt-in real Docker-backed managed-service coverage that follows Postgres logs, verifies the PID-less handle, executes SQL, and stops the container through the Engine API
- opt-in real remote Postgres clone coverage using separate non-default source and destination host ports, the production `pg_dump`/`psql` source policy, and assertions over cloned schema plus row data
- opt-in real PostGIS runtime coverage for PostgreSQL 16, 17, and 18 that verifies the server major, expected PostGIS line, geometry/geography functions, and persistence after container recreation
- opt-in real Docker-backed Prisma snapshot metadata + restore coverage in `pkg/database`

## Example/Smoke Coverage

The bundled example adapters are now deterministic smoke targets. Current smoke coverage includes:
- repeated runs with cache hits
- watch-mode selective reruns
- watch-mode file pickup verification that starts watch mode, edits a real file, and asserts the watch cycle event plus the affected rerun
- daemon-backed flush readiness verification that starts watch mode on the `go-next-monorepo` example, edits a real file, runs `flush --json`, and asserts success only arrives after the affected service reruns
- service readiness via ready-file probes on the example backend/frontend services
- DB snapshot reuse and dedicated postgres port isolation in the fake-DB example path
- multi-worktree DB and port isolation
- a second multi-service workflow shape with API, worker, and frontend services that exercises broader downstream restart behavior

The deterministic examples remain synthetic and local-only on purpose so tests stay deterministic and non-flaky.

Most Docker-backed integration coverage is intentionally opt-in locally. Enable the full suite with:

```bash
DEVFLOW_E2E_DOCKER=1 go test ./pkg/database -run Docker
```

On Apple Silicon, the opt-in suite queries Engine architecture through the Go API and requires `aarch64` or `arm64` to match the native Go host. It then exercises the same native multi-architecture Postgres and Alpine images used by normal managed-database workflows. The focused required Docker job also starts a managed runtime service, consumes a real Postgres log line, and stops it without an adapter wrapper process. If Docker Desktop is installed but its Engine is stopped, the test skips with a daemon-not-ready reason; the `docker` executable is neither checked nor invoked. The normal Windows matrix executes context and native named-pipe construction tests and compiles the complete SDK path; GitHub-hosted Windows runners do not provide a dependable Linux-container Docker daemon, so real Postgres/PostGIS execution remains on native Linux amd64/arm64 plus local/self-hosted Docker Desktop coverage.

The remote-clone case additionally requires host `pg_dump` and `psql` clients compatible with the default Postgres image. It starts distinct source and destination containers on dynamically allocated non-default host ports, seeds the source, runs `PostgresDumpSourcePolicy`, and verifies the destination schema and rows. The required Linux Docker job installs those clients and runs this case on both amd64 and arm64. On Apple Silicon with Homebrew, install the clients with `brew install libpq` and make sure `$(brew --prefix libpq)/bin` is on `PATH` before running the suite.

The PostGIS case uses the same architecture-aware production flavor as adapters and runs subtests for PostgreSQL 16, 17, and 18. It pulls each maintained PostGIS image on amd64 and exercises each bundled package-based native build on arm64. Every subtest verifies the actual server major, the exact PostGIS line encoded in the amd64 tag (and a functional packaged PostGIS 3.x on arm64), geometry construction, geography distance calculation, and a spatial row after deleting/recreating the container while retaining its named volume. The ARM package repository can publish a newer PostGIS minor line than the fixed amd64 image tag, so cross-architecture equality is not an invariant. The persistence check specifically covers PostgreSQL 18's changed `/var/lib/postgresql` mount target. The first arm64 run may take several minutes because it builds three local images. Unlike the rest of the Docker suite, this focused case is also a required GitHub Actions job on native Ubuntu amd64 and Ubuntu arm64. Run the same command locally on Apple Silicon with Docker Desktop to cover the macOS/Docker Desktop integration layer that GitHub-hosted macOS runners cannot provide.

There is also now a real `embedded-web-app` adapter with:
- unit coverage for graph shape and env finalization
- graph coverage proving its managed Postgres service has no Docker CLI task or prerequisite
- manual smoke validation against a local embedded-frontend Go app repo
- verified `build-all` execution through the real repository
- managed Postgres logs and shutdown delegated to the Docker Engine API rather than `docker info`/`docker logs` subprocesses

The current example coverage splits cleanly across three shapes:
- `go-next-monorepo`: deterministic frontend + backend + DB flow
- `web-worker-workspace`: deterministic API + worker + frontend multi-service flow
- `embedded-web-app`: real repository adapter for a Go server + embedded frontend + dedicated Postgres flow

All three real-DB paths use `pkg/database` Engine APIs for container service logs, shutdown, and in-container SQL; source examples must not teach `docker info`, `docker logs`, or `docker exec` subprocesses.
