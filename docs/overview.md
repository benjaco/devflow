# Overview

Devflow is a local-first runner for development DAGs. It executes cacheable one-shot tasks, supervises long-running services, isolates concurrent worktrees, and exposes stable JSON output so the same surface can serve humans, CI, and later agent wrappers.

The current implementation provides the generic engine layers first:
- graph validation and traversal
- task fingerprinting
- snapshot-based local cache
- process supervision
- instance and port management
- JSON CLI contracts

Future milestones add incremental watch execution, a richer TUI, and a more realistic example adapter.
