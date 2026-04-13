# CLI

Implemented commands:

- `devflow` (default launcher behavior)
- `devflow run <target>`
- `devflow watch <target>`
- `devflow restart <task>`
- `devflow stop`
- `devflow cache status`
- `devflow cache invalidate`
- `devflow cache gc`
- `devflow status`
- `devflow logs <task>`
- `devflow instances`
- `devflow doctor`
- `devflow tui`
- `devflow graph list`
- `devflow graph show <target>`
- `devflow graph affected --files ...`

All implemented commands support `--json`.

Running bare `devflow` now acts as the default operator entry path:
- it uses the repo-local launcher script
- rebuilds the local binary when the `devflow` source tree is newer than `.devflow/bin/devflow`
- auto-detects the current worktree project when possible
- chooses the project's preferred default target (`up`, `fullstack`, or the adapter-defined default)
- if no detached supervisor is live for the current worktree, starts that target detached
- opens the TUI for the current worktree

`run` provisions an instance, executes the target closure, restores cacheable one-shot tasks when possible, and keeps supervised services alive until interrupted.

Implemented `run` flags include:
- `--json`
- `--ci`
- `--watch`
- `--detach`
- `--worktree`
- `--project`
- `--max-parallel`

`watch` runs an initial watch-mode cycle, then keeps polling for changes and reruns only the affected downstream slice. In JSON mode it emits the typed event stream line-by-line.

`watch` also supports `--detach`.

`restart` supports rerunning non-service task slices from the CLI. For service tasks, if the instance was started with a detached run, `restart` stops the detached supervisor and relaunches the last detached target.

`stop` terminates persisted service PIDs for a selected task or, when used with `--all`, terminates the detached supervisor for the instance and updates persisted node state to `stopped`.

`status` now reports instance metadata in both text and JSON forms, including:
- worktree
- target and mode
- assigned ports
- sanitized DB details
- derived local URLs such as `backend`
- detached supervisor PID/liveness/log path when present

`logs` supports task logs as before and also accepts `supervisor` to read the detached supervisor log directly.

`tui` now opens a live operator console for an existing instance. The first slice includes:
- instance/runtime header
- live task list with selection
- selected-task metadata
- live tail of the selected task log
- toggle to the detached supervisor log
- running tasks pinned first and pending work directly below them
- `i` on the selected task invalidates the selected downstream cacheable slice and relaunches the current target

Implemented `tui` flags include:
- `--worktree`
- `--instance`

`cache status` lists cache entries, `cache invalidate` removes entries globally or per task, and `cache gc` keeps only the newest N entries per task.
