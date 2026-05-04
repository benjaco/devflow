# Testing

Devflow uses three testing layers:

## Unit Tests

- graph validation and closures
- fingerprint determinism
- cache snapshot and restore semantics
- cache-key override stability and correctness
- instance identity and env persistence
- port allocation and reuse

## Integration Tests

- subprocess stdout/stderr capture
- per-task log truncation so logs reflect the current run attempt
- interactive prompt detection and answer forwarding with a real prompt CLI fixture
- service lifecycle management
- CI-mode service targets act as readiness probes and stop services before returning
- daemon lifecycle coverage for per-worktree daemon startup locking, CLI connection, daemon event persistence/fanout, and preserving legacy supervisor/executor PIDs for later cleanup
- stop cleanup for daemon-owned work, legacy supervisor/executor refs, tracked service, and stale status process groups
- `stop --all` cleanup for the instance-managed database container while preserving the volume
- service readiness success and timeout behavior
- built-binary helper build/run/start coverage and cache-restore coverage
- database runtime command planning and snapshot-manifest coverage
- builder API coverage for command tasks, services, target definitions, automatic cacheability from outputs, port env references, dotenv loading, path inputs, and glob inputs
- database component coverage for common Postgres/Prisma task generation, optional snapshot-root defaults, instance DB/env finalization, and explicit non-cacheable migration authoring
- database runtime host-port readiness and stale published-port reconciliation coverage
- managed migration workflow coverage for exact snapshot reuse, nearest-prefix restore, incompatible base fingerprint misses, changed-latest tail replay, per-migration prefix snapshots, checked remote Postgres clone policy failures, Prisma directory-only migration inspection, Prisma lock-file churn, model-free schemas without migrations, added/deleted Prisma migrations, changed older Prisma migration rebuilds, failed Prisma migration apply without snapshotting, Prisma prefix deploy snapshots, fresh/no-migration migration-needed errors, schema-without-migration migration-needed errors, Prisma authoring database reconciliation, and Prisma generate helpers
- Prisma schema/migration inspection and nearest-prefix snapshot planning coverage
- dotenv parsing and merged runtime-env coverage with devflow-managed DB overrides
- CLI JSON output shape, including command-level lifecycle coverage for `run`, `status`, `logs`, `instances`, `doctor`, and `stop`
- scoped docs command coverage for `devflow docs setup`, `devflow docs development`, and the bare `devflow docs` usage error
- CLI help coverage for important `run` flags such as `--ci`, `--detach`, and `--watch`
- Go-first release command coverage for `version` and `upgrade`, with `upgrade` tested through a fake `go` executable on `PATH`
- installed/source bootstrap coverage for generated local modules, source `replace` directives, project-file rebuilds, stable binary reuse, and concurrent localbuild serialization
- global cache coverage for the single OS user cache root and project cache namespaces
- required CLI detection, target-scoped required CLI selection, and platform-script install coverage in `pkg/project` and `internal/cli`
- engine-level interactive prompt event plus answer-file integration coverage
- TUI database/Prisma panel rendering, drift warning, Prisma snapshot-summary loading, daemon-backed migration target action, detached-target relaunch, and progress/footer status coverage
- sequential engine execution with cache hits
- distinct canceled-vs-failed task-state behavior when sibling task failure cancels in-flight work, plus explicit `migration_needed` task-state classification for database migration authoring guards
- scheduler error preservation so a canceled sibling does not replace the first actionable task failure with `context canceled`
- polling watch batching and selective watch reruns
- graph affected explanations for path, file, directory, glob, ignored, and unmatched paths
- watch cascade pruning so downstream tasks do not run past warmups or services that are blocked from watch execution, including full watch execution and mixed blocked/allowed branch coverage
- watch service restart policies, including `RestartAlways` selection and full watch execution behavior
- flush coordination coverage for request/ack path generation, watcher inclusion of the flush sync directory under `.devflow`, engine ack timing after reruns and sync-only batches, failed-task ack issues, service readiness health issues, CLI daemon/timeout behavior, and sync-sentinel retouch while waiting for an ack
- opt-in real Docker-backed database runtime snapshot/restore coverage in `pkg/database`
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

Docker-backed integration coverage is intentionally opt-in. Enable it with:

```bash
DEVFLOW_E2E_DOCKER=1 go test ./pkg/database -run Docker
```

There is also now a real `embedded-web-app` adapter with:
- unit coverage for graph shape and env finalization
- manual smoke validation against a local embedded-frontend Go app repo
- verified `build-all` execution through the real repository
- verified early failure when Docker is installed but the daemon is not running

The current example coverage splits cleanly across three shapes:
- `go-next-monorepo`: deterministic frontend + backend + DB flow
- `web-worker-workspace`: deterministic API + worker + frontend multi-service flow
- `embedded-web-app`: real repository adapter for a Go server + embedded frontend + dedicated Postgres flow
