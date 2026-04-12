# CLI

Implemented commands:

- `devflow run <target>`
- `devflow status`
- `devflow logs <task>`
- `devflow instances`
- `devflow doctor`
- `devflow graph list`
- `devflow graph show <target>`
- `devflow graph affected --files ...`

All implemented commands support `--json`.

`run` provisions an instance, executes the target closure, restores cacheable one-shot tasks when possible, and keeps supervised services alive until interrupted.

Implemented `run` flags include:
- `--json`
- `--ci`
- `--watch` (mode selection only; full watch behavior is still pending)
- `--worktree`
- `--project`
- `--max-parallel`
