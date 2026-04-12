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
- CLI JSON output shape
- sequential engine execution with cache hits
- polling watch batching and selective watch reruns

## Example/Smoke Coverage

The bundled example adapter is now a deterministic full-stack-style smoke target. Current smoke coverage includes:
- repeated runs with cache hits
- watch-mode selective reruns
- service readiness via ready-file probes on the example backend/frontend services
- multi-worktree DB and port isolation

The example remains synthetic and local-only on purpose so tests stay deterministic and non-flaky.
