# CLI

Implemented commands:

- `devflow` (default launcher behavior)
- `devflow run <target>`
- `devflow watch <target>`
- `devflow flush [target]`
- `devflow restart <task>`
- `devflow stop`
- `devflow action list`
- `devflow action run <action-id>`
- `devflow migration create <name>`
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
- `devflow docs setup`
- `devflow docs development`
- `devflow graph list`
- `devflow graph show <target>`
- `devflow graph affected --files ...`

All implemented commands support `--json` except `devflow docs setup` and `devflow docs development`, which intentionally print plain bundled user Markdown only.

Running bare `devflow` now acts as the default operator entry path:
- it can be the installed Go binary or the repo-local launcher script
- the repo-local launcher rebuilds the bootstrap binary when the content build key for the core `devflow` source tree changes
- requires `./devflow.project.go` in the selected worktree
- compiles a worktree-local binary into `<worktree>/.devflow/bin/devflow-local` when the project file or Devflow version/source inputs are newer
- `exec`s into that worktree-local binary for all normal commands
- chooses the project's preferred default target (`up`, `fullstack`, or the adapter-defined default)
- ensures the per-worktree daemon is running
- if no daemon-owned watch loop is active for that target, starts the default target in daemon-owned watch mode
- opens the TUI for the current worktree
- if this bare TUI launch created the daemon, quitting the TUI stops active daemon-owned work with the normal `stop --all` path and shuts the daemon down; reconnecting to an already-running daemon leaves it alive on quit

There is currently no built-in adapter fallback. Missing `devflow.project.go` is a hard error.

`run` provisions an instance, executes the target closure, and restores cacheable one-shot tasks when possible.

Service lifecycle contract:
- attached non-CI `devflow run <target>` connects to the per-worktree daemon, waits for service readiness, and then keeps supervised services alive until interrupted or until a service exits
- if a service exits during attached `run`, the command returns a service-exited error
- `devflow run <target> --ci --json` is finite and deliberately bypasses the daemon; service tasks are started, readiness is checked, services are stopped, and status records those services as `stopped`
- `devflow run <target> --detach --json` returns after asking the daemon to launch the target; it is not a health/readiness gate
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

`watch` connects to the per-worktree daemon, runs an initial watch-mode cycle, then keeps polling for changes and reruns only the affected downstream slice. In attached JSON mode it emits the typed event stream line-by-line.

Watch file matching is driven by adapter task inputs. Changed files directly affect tasks whose `Inputs.Files`, `Inputs.Dirs`, `Inputs.Globs`, or `Inputs.Filtered` paths match the changed paths, then the engine cascades through downstream tasks that are eligible to rerun in watch mode.

The watcher is scoped to declared inputs in the selected target closure plus Devflow's flush sync directory. This keeps idle watch daemons from recursively polling unrelated dependency trees such as `node_modules`. If a project truly needs to watch a normally ignored directory, declare it as an input path.

Watch cascades respect dependency barriers. If an intermediate task in the affected slice is not allowed to run in watch mode, downstream tasks past that intermediate are not run in that cycle.

`graph affected --files a,b --explain --json` reports why changed files do or do not affect tasks. Explanations include direct file matches, directory matches, glob matches, filtered matches, ignored paths, and unmatched files. This is the primary debugging tool for generated-output watch loops.

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

`flush` is the AI readiness gate for detached watch workflows. It makes sure the per-worktree daemon is running a `watch` loop for the selected target, writes a flush request plus a sync sentinel, waits until the watcher acknowledges that sentinel after the current watch batch settles, and then returns the target-closure health result.

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
- without a positional target, a live daemon watch loop reuses `inst.LastRun.Target`
- without a live watch loop, `inst.LastRun.Target` is reused when present
- otherwise the project preferred target is used

Daemon behavior:
- no daemon-owned watch loop: starts `devflow watch <target> --detach` through the daemon
- live daemon watch loop for the same target: reused
- live daemon watch loop for a different target: fails with `target_mismatch`
- live daemon non-watch work: fails with `non_watch_supervisor`

`flush --json` returns `FlushResult` with the request ID, instance ID, worktree, project, target, mode, whether a daemon watch loop was started, sync/health success, node states, service health, and structured issues. The command exits non-zero when `success=false`, including timeout and health-check failures.

`action` is the generic foreground operation surface for explicit project operations that are not normal DAG targets. Actions are discovered from the project adapter through the daemon.

Usage:

```bash
devflow action list
devflow action list --json
devflow action run <action-id-or-alias>
devflow action run <action-id-or-alias> --input name=value --json
devflow action run --kind devflow.database.migration.create --component prisma --name add_user
```

`action list --json` returns the project name plus registered action specs, including stable action ID, semantic kind, category, component, input schema, effects, relaunch policy, and aliases. `action run --json` returns an action result with action ID, kind, status, inputs, created files discovered from declared write effects, the underlying run result when the action is task-backed, and relaunch metadata when the action restarts the previous daemon target.

`migration create` is a convenience command over the standard action kind `devflow.database.migration.create`.

Usage:

```bash
devflow migration create add_user
devflow migration create add_user --component prisma
devflow migration create add_user --json
```

If exactly one migration-create action exists, the component flag can be omitted. If several migration systems are registered, `--component` disambiguates. Migration creation is never inferred from targets such as `new-migration`; adapters must register actions.

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

`upgrade --version v0.1.2` installs that specific tag. `upgrade --direct` sets `GOPROXY=direct` for testing freshly pushed commits before the public Go proxy catches up. `upgrade --json` returns the command, package, version target, success flag, duration, and any error/output. It exits non-zero when the underlying `go install` fails. In text mode, `upgrade` warns when `go install` writes a binary somewhere other than the `devflow` command currently found on `PATH`.

`docs setup` prints the setup/pipeline user docs bundle. `docs development` prints the day-to-day CLI/TUI/operator user docs bundle.

Bare `docs` is intentionally a usage error so agents and users do not accidentally pull both context lanes into one prompt. The docs commands are projectless, have no flags, have no JSON mode, and do not print contributor docs.

`restart` connects to the daemon. It supports rerunning non-service task slices from the CLI. For service tasks, if the instance has a recorded run target, `restart` asks the daemon to relaunch that target.

`stop` is daemon-backed; if no daemon is running, it may start a short-lived daemon to reconcile persisted runtime state. It terminates persisted service PIDs for a selected task. With `--all`, it reconciles all known runtime process groups for the instance: active daemon-owned work, legacy supervisor/executor PIDs and their process-tree descendants, tracked service tasks, and PID-bearing status nodes. It then clears persisted process refs, updates nonterminal node state to `stopped`, stops the managed database container, and shuts down the daemon after sending the response.

`doctor` supports `--target <target>`. Without a target it checks the full adapter required CLI catalog. With a target it resolves the target or task name and checks only required CLIs attached through `RequiredCLIs` to that target and its task closure. JSON includes `project`, `target`, and `cliScope`.

`clis status` reports adapter-defined required CLIs, whether they are already installed, and whether a platform install script is available. `clis status --target <target>` uses the same CLI scope as target-scoped doctor. JSON includes `requiredCLIs`; the older `dependencies` field is still emitted for compatibility.

`clis install` runs adapter-defined install scripts only for missing required CLIs and then re-checks that each installed command is now available on `PATH`. `clis install --target <target>` installs only CLIs needed for that target closure. `deps status/install` remains available as a compatibility alias.

`status` is read-only: it uses a live daemon when one is already running, otherwise it reads the persisted instance/status files without starting a daemon. It reports instance metadata in both text and JSON forms, including:
- worktree
- target and mode
- assigned ports
- sanitized DB details
- derived local URLs such as `backend`
- daemon/supervisor PID, liveness, and log path when present
- per-node debug metadata for `debug_service` tasks, including host, port, port name, binary path, package, protocol, and a Go remote-attach shape

Task states now distinguish:
- `failed`: the task itself failed
- `migration_needed`: the task intentionally blocked because a database migration must be authored before downstream work can run
- `canceled`: the task was interrupted because another task failed or the run was canceled

`logs` supports task logs as before and also accepts `supervisor` to read the daemon/supervisor log directly.

Task log files now represent the current run attempt for that task. The engine truncates the log at task-attempt start before adapter code can emit progress, and subprocess output appends within that attempt. Older successful, failed, or canceled output must not stay mixed into a newer running attempt.

`tui` now opens a live operator console connected to the per-worktree daemon. Without `--instance`, `devflow tui` follows the same default launch path as bare `devflow`: resolve the default target, ensure the per-worktree daemon is running it in watch mode, wait for a matching non-empty status snapshot, then render. With `--instance`, `tui` is attach-only and does not start or retarget work.

The first slice includes:
- instance/runtime header
- live task list with selection
- selected-task metadata
- live tail of the selected task log
- toggle to the daemon/supervisor log
- `d` toggles a database/Prisma panel with managed Postgres identity, persisted flavor (`postgres` or `postgis`), the selected PostgreSQL major when configured, configured/automatic image selection, and recent cached Prisma migration-prefix snapshots; `F2` is a backup key
- the database/Prisma panel flags schema/migration drift and `m` asks for a migration name, then sends a daemon action with kind `devflow.database.migration.create` through the daemon-owned engine and relaunches the previously detached target; `F4` is a backup key
- while the TUI creates a Prisma migration, the footer status reports target/task state and the latest task output line
- global shortcuts are disabled while text-input popups are focused, so migration names can contain normal letters
- running tasks pinned first and pending work directly below them
- `i` on the selected task invalidates the selected downstream cacheable slice and relaunches the current target
- `t` on the selected task updates the detached run target to that task and relaunches the instance on the selected task closure
- popup confirm and text prompts for interactive tasks that emit `interaction_requested` events
- primary live refresh from the daemon event subscription, with the persisted event stream at `.devflow/state/instances/<instance-id>/events.jsonl` as fallback

Daemon ownership is session-scoped. If `devflow tui` or bare `devflow` has to start the daemon for that TUI session, quitting the TUI sends `stop --all` through the daemon so services, managed databases, and the daemon exit together. If the daemon already existed before the TUI connected, quitting the TUI only closes the UI.

Interactive prompt answers are written back through the instance interaction directory, so detached runs can still receive operator input from the TUI.

Implemented `tui` flags include:
- `--worktree`
- `--instance`

`cache status` lists entries for the selected project cache namespace, `cache invalidate` removes entries for that namespace globally or per task, and `cache gc` keeps only the newest N entries per task in that namespace. Task cache storage is physically global under the OS user cache directory, but entries are grouped by project namespace.
