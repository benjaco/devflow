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
- service readiness success and timeout behavior
- built-binary helper build/run/start coverage and cache-restore coverage
- database runtime command planning and snapshot-manifest coverage
- Prisma schema/migration inspection and nearest-prefix snapshot planning coverage
- dotenv parsing and merged runtime-env coverage with devflow-managed DB overrides
- CLI JSON output shape, including command-level lifecycle coverage for `run`, `status`, `logs`, `instances`, `doctor`, and `stop`
- dependency detection and platform-script install coverage in `pkg/project` and `internal/cli`
- engine-level interactive prompt event plus answer-file integration coverage
- sequential engine execution with cache hits
- distinct canceled-vs-failed task-state behavior when sibling task failure cancels in-flight work
- polling watch batching and selective watch reruns
- watch cascade pruning so downstream tasks do not run past warmups or services that are blocked from watch execution, including full watch execution and mixed blocked/allowed branch coverage
- opt-in real Docker-backed database runtime snapshot/restore coverage in `pkg/database`
- opt-in real Docker-backed Prisma snapshot metadata + restore coverage in `pkg/database`

## Example/Smoke Coverage

The bundled example adapters are now deterministic smoke targets. Current smoke coverage includes:
- repeated runs with cache hits
- watch-mode selective reruns
- watch-mode file pickup verification that starts watch mode, edits a real file, and asserts the watch cycle event plus the affected rerun
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
