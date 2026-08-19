# Devflow Development Docs

Use this for day-to-day project development after Devflow is already integrated.

For setup and pipeline-authoring docs, use:

```bash
devflow docs setup
```

## Daily Workflow

Typical human workflow:

```bash
devflow
devflow status --json
devflow logs app
devflow stop --all --json
```

Typical agent workflow:

```bash
devflow watch up --detach --json
# edit files
devflow flush up --json
# run focused tests only after success=true
```

If `flush` fails, inspect `issues`, `nodes`, `services`, and referenced log paths before retrying.

## Service Lifecycle

Use the lifecycle command that matches the job:

- Dev/watch/operator commands use one daemon per worktree. The daemon owns file watching, services, status, and live TUI updates. Sibling worktrees get separate daemons but still share the global task cache.
- `devflow run up` is foreground/attached through that daemon. It starts the target closure, waits for service readiness, then keeps the terminal attached while services run. Interrupt it from that terminal when you are done.
- `devflow run up --ci --json` is a readiness probe for CI-style validation and does not use the daemon. If the target contains services, Devflow starts them, waits for readiness, stops them, and returns a finite result. Progress and task output stream to stderr while stdout remains one final JSON document. It is not a background dev environment.
- `devflow run up --detach --json` asks the daemon to start the target and returns after launch. It does not prove the whole target is healthy; use `status`, `logs`, or a watch `flush` workflow when readiness matters.
- `devflow watch up --detach --json` asks the daemon to start the recommended dev loop for humans and agents.
- `devflow flush up --json` is the readiness gate for daemon-owned watch mode. It waits for file-change work to settle and checks in-chain services.
- `devflow stop --all --json` stops daemon-owned work, stale process records, the managed database container, and the worktree daemon. It preserves the database volume. The next `devflow` or `watch` command starts a fresh daemon.

After `stop --all`, `status --json` may still include a `db` object. That object is the desired managed database identity and connection metadata for the instance, not proof that the container is currently running.

For finite check/test targets that depend on services such as Postgres or a local app, generally use `devflow run <target> --ci --json` so Devflow starts the services as readiness probes and stops them before returning.

For AI-assisted development, prefer `watch --detach` plus `flush` over an attached service `run`. Attached runs are useful for a human terminal, but they are not a clean "start and return when ready" automation interface.

## Pipeline Validation

When changing task inputs, outputs, or dependencies, validate a finite target before trusting its cache behavior:

```bash
devflow validate build --mode artifacts --json
devflow validate build --mode orders --max-orders 1000 --json
devflow validate build --mode all --json
```

Artifact validation gives each task only its declared worktree inputs and upstream declared outputs, then reports undeclared writes and missing outputs. Order validation runs every legal topological order sequentially and requires identical final declared artifacts. A failed order usually identifies a missing dependency edge; an output mismatch means nominally independent tasks still affect each other.

Order validation is genuinely exhaustive within `--max-orders`. If the graph has more orders than the bound, it returns `complete=false` and runs no permutations. Raise the limit only when the repeated task executions are acceptable.

Validation uses disposable worktree sandboxes, bypasses caches/stamps, and rejects service/debug-service targets. `.git` and `.devflow` are not copied. External effects such as databases, networks, global caches, absolute paths, and unregistered background processes are outside that filesystem sandbox, so use a finite target that is safe to repeat.

## Go Debugging

If the adapter exposes a debug target with `GoDebugService`, start it like any other dev target:

```bash
devflow watch debug --detach --json
devflow status --json
```

The debug node in `status --json` includes the stable localhost debug port and an attach configuration shape. Point VS Code or Cursor's Go debugger at that host/port with remote attach. Devflow owns the outer lifecycle: on relevant file changes it stops Delve, rebuilds the debug binary, restarts Delve on the same named port, and then waits for debugger/app readiness.

Editor reconnect after a Delve process replacement is editor-dependent. The stable port keeps manual re-attach cheap, but Devflow does not promise invisible debugger reconnection after every restart.

## TUI

Run `devflow` or `devflow tui` with no args in a project worktree to start or reconnect to the worktree daemon, ensure the default target is running in watch mode, and open the TUI. This is the normal day-to-day entry point when you want edits to cascade automatically.

Use `devflow tui --instance <id>` only when you intentionally want to attach to a specific existing instance. In that attach-only mode, Devflow does not start or retarget background work for you.

If that TUI launch had to start the worktree daemon, quitting the TUI also stops daemon-owned work and exits the daemon. If a daemon was already running before the TUI connected, quitting only closes the UI; after terminal restoration DevFlow prints exact status/stop commands for the background workflow that remains alive.

Useful TUI keys:
- `?`: contextual help for the current view
- `Tab`: switch task-table/log-pane focus
- `q`: quit
- `j` / `k` / arrow keys: move in the focused pane
- `Home` / `End`: task top/bottom or log beginning/resume-follow, depending on focus
- `f`: resume live-log following
- `o`: load an older bounded block of retained log lines
- `l`: selected task log or supervisor log
- `d`: database/Prisma panel
- `a`: toggle the explicit active/failure attention view
- `m`: create a migration through the project migration-create action
- `i`: immediately invalidate and rerun the selected task scope
- `t`: choose a target, then preview and confirm retarget scope

Running logs open at the live tail as `FOLLOWING`. Page Up or upward scrolling changes the view to `PAUSED`, preserves that manual position across refreshes, and never snaps back; End or `f` resumes at the latest line. The panel reports its retained line range and truncation. `o` increases the bounded retained window without reading an unbounded file into memory. Switching log sources resets following predictably.

On wide terminals (at least 120 columns and 24 rows), the task selector stays on the left and the selected log fills the workspace on the right. Narrower terminals retain the stacked task-over-log layout, and unusually short terminals prioritize task navigation before showing the optional log pane.

Task rows remain in stable graph order while states change. The selected task/log source and focused pane are always labeled, lifecycle badges remain distinct without color, and failed/blocked/degraded rows include a concise reason. Action status has reserved footer space and remains visible until replaced or dismissed. Rerun immediately submits the daemon-scoped invalidation without confirmation. Retarget shows the daemon's stop/execute/preserve/restart plan, and Escape cancels before any process changes.

The database/Prisma panel shows managed Postgres identity, the persisted `postgres`/`postgis` flavor and configured/automatic image choice, cached Prisma migration-prefix snapshots, and schema/migration drift when Prisma metadata is available. Migration authoring is explicit; normal startup does not secretly generate migrations. When you create a migration from the TUI, Devflow sends a daemon action with kind `devflow.database.migration.create`, streams task progress in the footer immediately, surfaces any declared confirmation prompts, and then relaunches the previously detached target so services come back through the graph. The same `m` action can drive non-Prisma components such as PayloadCMS when the adapter registers a migration-create action.

The same action is available from the CLI:

```bash
devflow action list
devflow migration create add_user
devflow migration create add_user --component prisma --json
```

Use `--component` when a project has more than one migration system.

## Watch Mode

Watch mode maps changed files to task inputs, then reruns the affected downstream slice.

Devflow watches the declared input paths for the selected target closure, not the whole project tree. This keeps folders such as `node_modules` out of the idle watch loop. If edits are not being picked up, add the missing source path to the relevant task inputs and check it with `graph affected --explain`.

Run:

```bash
devflow watch up --detach --json
```

Then after edits:

```bash
devflow flush up --json
```

`flush` proves the watcher has processed a sync sentinel written after your edits. It returns success only when the selected target closure has settled and in-chain services are healthy.

Important watch rules:
- downstream jobs do not run past blocked intermediate tasks
- services default to affected-slice restarts
- `RestartNever` prevents watch restarts
- `RestartAlways` restarts a service on any watch cycle that affects the target

Use `graph affected --explain` when a file change restarts too much or too little:

```bash
devflow graph affected --files internal/storage/sqlc/users.sql.go --explain --json
```

The explanation shows which task input matched the file, including filtered semantic inputs, which ignore pattern suppressed it, and which files did not match any task.

## Status And Logs

Use JSON status for automation and plain logs for fast inspection:

```bash
devflow status --json
devflow logs app
devflow logs supervisor
```

A failed `run --ci --json` already includes `error`, `failedNode`, `failedNodeLogPath`, a bounded `logTail`, every selected node's final run snapshot, and cache hit/miss lists. Each node reports duration and cache timing when applicable. Use the referenced full log only when the bounded tail is insufficient. Task, daemon, and event logs are owner-only on Unix-like systems.

For flush failures, start with the JSON `issues`, then inspect the referenced task logs. Do not rerun downstream tests until the relevant flush target reports `success=true`.

## Required CLIs And Doctor

For prerequisite checks, prefer the same target scope you will execute:

```bash
devflow doctor --target up --json
devflow doctor --target up --strict --json
devflow clis status --target up --json
```

Target-scoped doctor checks only include commands and required environment metadata attached to the selected target and its task closure, plus project-wide required env. The result's `requiredEnv` array shows whether each key came from the invoking process or project configuration. Use `--strict` in CI to retain the full report but exit nonzero when any check fails. Unrelated tools and task-only env keys do not block normal work.

Managed Postgres is an Engine service prerequisite rather than a CLI prerequisite. Devflow connects through the Docker Engine Go API and reports a connection error when the selected target first needs the database; it does not require or invoke the `docker` command. On Windows, start Docker Desktop in Linux-container mode; on macOS, start Docker Desktop normally.

Install missing tools when the adapter provides install scripts:

```bash
devflow clis install --target up
```

## Cache Operations

Most cache behavior is automatic. Use manual cache commands when generated artifacts are stale or when you need to force a rebuild:

```bash
devflow cache status --json
devflow cache path --json
devflow cache key --target build --json
devflow cache invalidate --task backend_build --json
devflow cache gc --json
```

`cache path` returns the supported OS-specific cache root and project namespace path. `cache key` returns an aggregate target key plus each cacheable/stamped task key, which is suitable for a CI cache key without duplicating Devflow's fingerprint logic.

The TUI `i` action previews targeted invalidation/restart scope before execution. For CLI automation, use `restart --preview --json` or `stop --preview --json` before the matching lifecycle command.

## Runtime State

Per-worktree state stays under the project worktree:

```text
.devflow/state/
.devflow/logs/
.devflow/bin/
.devflow/localbuild/
```

Do not commit `.devflow/`.

Sibling worktrees can have isolated instance state and logs while sharing the global task cache.
