# Testing

Devflow uses three testing layers:

## Unit Tests

- graph validation and closures
- fingerprint determinism
- cache snapshot and restore semantics
- instance identity and env persistence
- port allocation and reuse

## Integration Tests

- subprocess stdout/stderr capture
- service lifecycle management
- CLI JSON output shape
- sequential engine execution with cache hits
- polling watch batching and selective watch reruns

## Example/Smoke Coverage

The current example adapter is intentionally lightweight. It exercises the generic engine and CLI without hardcoding a specific full-stack app into the core.
