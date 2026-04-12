# CLI

Implemented commands:

- `devflow run <target>`
- `devflow watch <target>`
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
- `--watch`
- `--worktree`
- `--project`
- `--max-parallel`

`watch` runs an initial watch-mode cycle, then keeps polling for changes and reruns only the affected downstream slice. In JSON mode it emits the typed event stream line-by-line.
