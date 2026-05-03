# CLI

Implemented commands:

- `devflow` (default launcher behavior)
- `devflow run <target>`
- `devflow watch <target>`
- `devflow flush [target]`
- `devflow restart <task>`
- `devflow stop`
- `devflow cache status`
- `devflow cache invalidate`
- `devflow cache gc`
- `devflow status`
- `devflow logs <task>`
- `devflow instances`
- `devflow doctor`
- `devflow clis`
- `devflow deps` (compatibility alias for `devflow clis`)
- `devflow tui`
- `devflow version`
- `devflow upgrade`
- `devflow docs`
- `devflow graph list`
- `devflow graph show <target>`
- `devflow graph affected --files ...`

All implemented commands support `--json` except `devflow docs`, which intentionally prints plain bundled user Markdown only.

Running bare `devflow` now acts as the default operator entry path:
- it can be the installed Go binary or the repo-local launcher script
- the repo-local launcher rebuilds the bootstrap binary when the core `devflow` source tree is newer than the repo-local `.devflow/bin/devflow`
- requires `./devflow.project.go` in the selected worktree
- compiles a worktree-local binary into `<worktree>/.devflow/bin/devflow-local` when the project file or Devflow version/source inputs are newer
- `exec`s into that worktree-local binary for all normal commands
- chooses the project's preferred default target (`up`, `fullstack`, or the adapter-defined default)
- if no detached supervisor is live for the current worktree, starts that target detached
- opens the TUI for the current worktree

There is currently no built-in adapter fallback. Missing `devflow.project.go` is a hard error.

`run` provisions an instance, executes the target closure, and restores cacheable one-shot tasks when possible.

Service lifecycle contract:
- attached `devflow run <target>` waits for service readiness and then keeps supervised services alive until interrupted or until a service exits
- if a service exits during attached `run`, the command returns a service-exited error
- `devflow run <target> --ci --json` is finite; service tasks are started, readiness is checked, services are stopped, and status records those services as `stopped`
- `devflow run <target> --detach --json` returns after launching the detached supervisor; it is not a health/readiness gate
- use `devflow watch <target> --detach --json` plus `devflow flush <target> --json` when automation needs a detached environment that is proven settled and healthy
- finite check/test targets with service dependencies should generally use `devflow run <target> --ci --json`
- `devflow stop --all --json` also stops the instance-managed database container when one is recorded; it does not remove the Docker volume

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

`graph affected --files a,b --explain --json` reports why changed files do or do not affect tasks. Explanations include direct file matches, directory matches, ignored paths, and unmatched files. This is the primary debugging tool for generated-output watch loops.

`Inputs.Ignore` uses the same path-matching model for fingerprinting and watch matching:
- exact or glob matches use slash-normalized paths
- a pattern also suppresses descendants when the changed path has that pattern as a path prefix
- for directory inputs, ignore patterns are checked both root-relative and relative to the input directory
- for explicit file inputs, root-relative ignore patterns can suppress that file

For service restart policies, `RestartNever` blocks watch restarts, `RestartOnInputChange` follows the affected downstream slice, and `RestartAlways` restarts the service on any watch cycle that affects the selected target.

For watch-cycle events:
- `files` is the raw changed file list from the watcher batch
- `affectedTasks` is the directly affected task list derived from those file changes

`watch` also supports `--detach`.

`flush` is the AI readiness gate for detached watch workflows. It makes sure a detached `watch` supervisor is running for the selected target, writes a flush request plus a sync sentinel, waits until the watcher acknowledges that sentinel after the current watch batch settles, and then returns the target-closure health result.

Usage:

```bash
devflow flush [target]
devflow flush [target] --json
devflow flush [target] --worktree <path>
devflow flush [target] --instance <id>
devflow flush [target] --project <name>
devflow flush [target] --timeout 60s
devflow flush [target] --max-parallel <n>
```

Target resolution:
- a positional `target` wins
- without a positional target, a live detached watch supervisor reuses `inst.LastRun.Target`
- without a live supervisor, `inst.LastRun.Target` is reused when present
- otherwise the project preferred target is used

Supervisor behavior:
- no detached supervisor: starts `devflow watch <target> --detach`
- live detached watch supervisor for the same target: reused
- live detached watch supervisor for a different target: fails with `target_mismatch`
- live detached non-watch supervisor: fails with `non_watch_supervisor`

`flush --json` returns `FlushResult` with the request ID, instance ID, worktree, project, target, mode, whether a supervisor was started, sync/health success, node states, service health, and structured issues. The command exits non-zero when `success=false`, including timeout and health-check failures.

`version` prints the installed Devflow version. `version --json` returns:

```json
{
  "version": "v0.1.0",
  "modulePath": "github.com/benjaco/devflow",
  "goVersion": "go1.23.0",
  "vcsRevision": "...",
  "vcsTime": "..."
}
```

`upgrade` updates the installed command by running:

```bash
go install github.com/benjaco/devflow/cmd/devflow@latest
```

`upgrade --version v0.1.2` installs that specific tag. `upgrade --json` returns the command, package, version target, success flag, duration, and any error/output. It exits non-zero when the underlying `go install` fails.

`docs` prints all bundled user-facing Markdown files from `docs_users/` in sorted order.

It is projectless, has no flags, has no JSON mode, and does not print contributor docs.

`restart` supports rerunning non-service task slices from the CLI. For service tasks, if the instance was started with a detached run, `restart` stops the detached supervisor and relaunches the last detached target.

`stop` terminates persisted service PIDs for a selected task. With `--all`, it reconciles all known detached runtime process groups for the instance: supervisor, child `__internal_exec`, tracked service tasks, and PID-bearing status nodes. It then clears persisted supervisor/process refs and updates nonterminal node state to `stopped`.

`doctor` supports `--target <target>`. Without a target it checks the full adapter required CLI catalog. With a target it resolves the target or task name and checks only required CLIs attached through `RequiredCLIs` to that target and its task closure. JSON includes `project`, `target`, and `cliScope`.

`clis status` reports adapter-defined required CLIs, whether they are already installed, and whether a platform install script is available. `clis status --target <target>` uses the same CLI scope as target-scoped doctor. JSON includes `requiredCLIs`; the older `dependencies` field is still emitted for compatibility.

`clis install` runs adapter-defined install scripts only for missing required CLIs and then re-checks that each installed command is now available on `PATH`. `clis install --target <target>` installs only CLIs needed for that target closure. `deps status/install` remains available as a compatibility alias.

`status` now reports instance metadata in both text and JSON forms, including:
- worktree
- target and mode
- assigned ports
- sanitized DB details
- derived local URLs such as `backend`
- detached supervisor PID, child executor PID, liveness, and log path when present

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
- `d` toggles a database/Prisma panel with managed Postgres identity and recent cached Prisma migration-prefix snapshots
- the database/Prisma panel flags schema/migration drift and `m` asks for a migration name, then runs explicit Prisma migration generation from inside the TUI
- running tasks pinned first and pending work directly below them
- `i` on the selected task invalidates the selected downstream cacheable slice and relaunches the current target
- `t` on the selected task updates the detached run target to that task and relaunches the instance on the selected task closure
- popup confirm and text prompts for interactive tasks that emit `interaction_requested` events
- primary live refresh from the persisted detached event stream at `.devflow/state/instances/<instance-id>/events.jsonl`

Interactive prompt answers are written back through the instance interaction directory, so detached runs can still receive operator input from the TUI.

Implemented `tui` flags include:
- `--worktree`
- `--instance`

`cache status` lists entries for the selected project cache namespace, `cache invalidate` removes entries for that namespace globally or per task, and `cache gc` keeps only the newest N entries per task in that namespace. Task cache storage is physically global under the OS user cache directory, but entries are grouped by project namespace.
