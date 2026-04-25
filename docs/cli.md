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
- `devflow deps`
- `devflow tui`
- `devflow graph list`
- `devflow graph show <target>`
- `devflow graph affected --files ...`

All implemented commands support `--json`.

Running bare `devflow` now acts as the default operator entry path:
- it uses the repo-local launcher script
- rebuilds the bootstrap binary when the core `devflow` source tree is newer than the repo-local `.devflow/bin/devflow`
- requires `./devflow.project.go` in the selected worktree
- compiles a worktree-local binary into `<worktree>/.devflow/bin/devflow-local` when the project file or core sources are newer
- `exec`s into that worktree-local binary for all normal commands
- chooses the project's preferred default target (`up`, `fullstack`, or the adapter-defined default)
- if no detached supervisor is live for the current worktree, starts that target detached
- opens the TUI for the current worktree

There is currently no built-in adapter fallback. Missing `devflow.project.go` is a hard error.

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

Watch file matching is driven by adapter task inputs. Changed files directly affect tasks whose `Inputs.Files` or `Inputs.Dirs` match the changed paths, then the engine cascades through downstream tasks that are eligible to rerun in watch mode.

Watch cascades respect dependency barriers. If an intermediate task in the affected slice is not allowed to run in watch mode, downstream tasks past that intermediate are not run in that cycle.

For watch-cycle events:
- `files` is the raw changed file list from the watcher batch
- `affectedTasks` is the directly affected task list derived from those file changes

`watch` also supports `--detach`.

`restart` supports rerunning non-service task slices from the CLI. For service tasks, if the instance was started with a detached run, `restart` stops the detached supervisor and relaunches the last detached target.

`stop` terminates persisted service PIDs for a selected task or, when used with `--all`, terminates the detached supervisor for the instance and updates persisted node state to `stopped`.

`deps status` reports adapter-defined command dependencies, whether they are already installed, and whether a platform install script is available.

`deps install` runs adapter-defined install scripts only for missing dependencies and then re-checks that each installed command is now available on `PATH`.

`status` now reports instance metadata in both text and JSON forms, including:
- worktree
- target and mode
- assigned ports
- sanitized DB details
- derived local URLs such as `backend`
- detached supervisor PID/liveness/log path when present

Task states now distinguish:
- `failed`: the task itself failed
- `canceled`: the task was interrupted because another task failed or the run was canceled

`logs` supports task logs as before and also accepts `supervisor` to read the detached supervisor log directly.

Task log files now represent the current run attempt for that task. They are truncated when a task starts again, so older successful output does not stay mixed into a newer failed or canceled attempt.

`tui` now opens a live operator console for an existing instance. The first slice includes:
- instance/runtime header
- live task list with selection
- selected-task metadata
- live tail of the selected task log
- toggle to the detached supervisor log
- running tasks pinned first and pending work directly below them
- `i` on the selected task invalidates the selected downstream cacheable slice and relaunches the current target
- `t` on the selected task updates the detached run target to that task and relaunches the instance on the selected task closure
- popup confirm and text prompts for interactive tasks that emit `interaction_requested` events
- primary live refresh from the persisted detached event stream at `.devflow/state/instances/<instance-id>/events.jsonl`

Interactive prompt answers are written back through the instance interaction directory, so detached runs can still receive operator input from the TUI.

Implemented `tui` flags include:
- `--worktree`
- `--instance`

`cache status` lists cache entries, `cache invalidate` removes entries globally or per task, and `cache gc` keeps only the newest N entries per task.
