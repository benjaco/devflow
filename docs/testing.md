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
- service lifecycle management
- service readiness success and timeout behavior
- built-binary helper build/run/start coverage and cache-restore coverage
- database runtime command planning and snapshot-manifest coverage
- Prisma schema/migration inspection and nearest-prefix snapshot planning coverage
- dotenv parsing and merged runtime-env coverage with devflow-managed DB overrides
- CLI JSON output shape, including command-level lifecycle coverage for `run`, `status`, `logs`, `instances`, `doctor`, and `stop`
- sequential engine execution with cache hits
- polling watch batching and selective watch reruns

## Example/Smoke Coverage

The bundled example adapter is now a deterministic full-stack-style smoke target. Current smoke coverage includes:
- repeated runs with cache hits
- watch-mode selective reruns
- service readiness via ready-file probes on the example backend/frontend services
- DB snapshot reuse and dedicated postgres port isolation in the fake-DB example path
- multi-worktree DB and port isolation

The example remains synthetic and local-only on purpose so tests stay deterministic and non-flaky.

There is also now a real `bikecoach` adapter with:
- unit coverage for graph shape and env finalization
- manual smoke validation against the local BikeCoach repo
- verified `build-all` execution through the real repository
- verified early failure when Docker is installed but the daemon is not running
