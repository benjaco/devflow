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

- `devflow run up` is foreground/attached. It starts the target closure, waits for service readiness, then keeps the terminal attached while services run. Interrupt it from that terminal when you are done.
- `devflow run up --ci --json` is a readiness probe for CI-style validation. If the target contains services, Devflow starts them, waits for readiness, stops them, and returns a finite result. It is not a background dev environment.
- `devflow run up --detach --json` starts a detached supervisor and returns after launch. It does not prove the whole target is healthy; use `status`, `logs`, or a watch `flush` workflow when readiness matters.
- `devflow watch up --detach --json` starts the recommended detached dev loop for humans and agents.
- `devflow flush up --json` is the readiness gate for detached watch mode. It waits for file-change work to settle and checks in-chain services.
- `devflow stop --all --json` cleans up the detached supervisor, child executor, tracked services, stale process records, and the managed database container for the worktree. It preserves the database volume.

After `stop --all`, `status --json` may still include a `db` object. That object is the desired managed database identity and connection metadata for the instance, not proof that the container is currently running.

For finite check/test targets that depend on services such as Postgres or a local app, generally use `devflow run <target> --ci --json` so Devflow starts the services as readiness probes and stops them before returning.

For AI-assisted development, prefer `watch --detach` plus `flush` over an attached service `run`. Attached runs are useful for a human terminal, but they are not a clean "start and return when ready" automation interface.

## TUI

Run `devflow` with no args in a project worktree to start or reconnect to the default detached target and open the TUI.

Useful TUI keys:
- `q`: quit
- `j` / `k` / arrow keys: move selection
- `g` / `G`: top/bottom
- `l`: selected task log or supervisor log
- `d`: database/Prisma panel
- `m`: create a Prisma migration when the database panel reports one is needed
- `i`: invalidate selected task and rerun downstream
- `t`: retarget to the selected task

The database/Prisma panel shows managed Postgres identity, cached migration-prefix snapshots, and schema/migration drift. Migration authoring is explicit; normal startup does not secretly generate migrations. When you create a migration from the TUI, Devflow first reconciles the managed database to the best compatible migration-prefix state, then runs the configured Prisma migration generator.

## Watch Mode

Watch mode maps changed files to task inputs, then reruns the affected downstream slice.

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

The explanation shows which task input matched the file, which ignore pattern suppressed it, and which files did not match any task.

## Status And Logs

Use JSON status for automation and plain logs for fast inspection:

```bash
devflow status --json
devflow logs app
devflow logs supervisor
```

For flush failures, start with the JSON `issues`, then inspect the referenced task logs. Do not rerun downstream tests until the relevant flush target reports `success=true`.

## Required CLIs And Doctor

For prerequisite checks, prefer the same target scope you will execute:

```bash
devflow doctor --target up --json
devflow clis status --target up --json
```

Target-scoped required CLI checks only include commands attached to the selected target and its task closure, so unrelated tools do not block normal work.

Install missing tools when the adapter provides install scripts:

```bash
devflow clis install --target up
```

## Cache Operations

Most cache behavior is automatic. Use manual cache commands when generated artifacts are stale or when you need to force a rebuild:

```bash
devflow cache status --json
devflow cache invalidate --task backend_build --json
devflow cache gc --json
```

The TUI `i` action is usually faster for targeted local invalidation because it invalidates the selected downstream slice and relaunches the active target.

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
