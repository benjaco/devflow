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
- `devflow cache key`
- `devflow cache path`
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
- `devflow validate <target>`

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
- in that CI/JSON mode, task state, cache hit/miss, and task-log progress streams to stderr while stdout remains exactly one final JSON document
- `devflow run <target> --detach --json` returns after asking the daemon to launch the target; it is not a health/readiness gate. The additive `accepted`, `supervisorStarted`, `ready`, and `state` fields distinguish daemon acceptance, supervisor startup, and the response-time `starting|ready|failed|degraded` target snapshot; the legacy `detached`, `pid`, and other launch fields remain present.
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
- `--cache-key-manifest` (finite `--ci` runs only)
- `--commit-changes` (finite `--ci` runs only)
- repeated `--commit-path <git-pathspec>`
- `--commit-message <message>`
- `--push`
- `--fail-after-commit`

The final `RunResult` includes top-level failure text, failed-node name and log path, an optional bounded terminal tail, `failureExcerpts`, cache hit/miss lists, optional `repositoryChanges`, and the final run snapshot of every selected node. Downstream work skipped after a dependency failure is `blocked` with the dependency in `lastError`; unrelated interrupted work is `canceled`. Each node includes `durationMs`; cacheable nodes also include cache outcome plus key/read/write/manifest/total timing. `failureExcerpts` scans the log as a stream and recognizes Go `--- FAIL:` blocks and `*_test.go:line:` diagnostics, panic/fatal output, compiler keywords or `file.go:line:column:` locations, `AssertionError`, conventional error/failed-test summaries, and process-failure markers. It keeps up to five context lines before and 30 after, merges nearby windows, removes overlap between adjacent windows, and is capped at five windows, 200 total lines, 64 KiB total text, and 8 KiB per line. A window is omitted if the aggregate cap cannot retain its triggering marker. For an early service exit with no recognized marker, one `process-exit-tail` window keeps the last 12 meaningful bounded lines. Excerpts and the terminal tail use the same environment-secret/PostgreSQL-URL redaction. Empty logs still produce `[]`.

### Atomic repository repair

`run --ci --commit-changes` turns a successful finite DAG into one tightly scoped repository repair transaction:

```bash
devflow run ci \
  --ci \
  --json \
  --commit-changes \
  --commit-path frontend \
  --commit-path ':(glob)backend/**/*.sql.go' \
  --commit-path ':(glob)backend/schemas/*.sql' \
  --commit-message 'bot(ci): automated DevFlow formatting and generation' \
  --push \
  --fail-after-commit
```

`--commit-changes` requires `--ci`, at least one repeated `--commit-path`, and a non-empty `--commit-message`. `--push`, `--fail-after-commit`, `--commit-path`, and `--commit-message` are invalid without `--commit-changes`. The selected Devflow worktree must be the Git worktree root, have an existing `HEAD`, and be completely clean before the engine starts, including tracked, staged, and untracked status entries.

After the complete DAG succeeds, Devflow verifies that `HEAD` and its branch/detached state still match the preflight baseline. Git itself interprets every supplied pathspec, including pathspec magic. Devflow lists candidate tracked and untracked changes only through those pathspecs, separately scans tracked status across the repository, and rejects tracked changes outside the permitted set. Untracked paths outside the supplied pathspecs are not staged or committed. A failed DAG skips all post-run Git inspection, staging, commit, and push work.

When permitted changes exist, Devflow runs Git directly without a command shell, stages with `git add -A -- <pathspecs...>`, and verifies that the staged set is stable and contains nothing outside the permitted Git match. It writes that exact index tree, creates one commit object, and advances `HEAD` with an old-SHA compare-and-swap. This exact-tree plumbing path does not invoke normal commit hooks or commit signing. A staging/commit failure restores the previously clean index while retaining worktree edits. If neither configured `user.name`/`user.email` nor complete role-specific Git identity environment variables are available, the new author and committer identities are derived from the corresponding identities on `HEAD`.

`--push` invokes the configured plain `git push` only after the local commit exists. A failed push does not roll back the commit: the final result is nonzero with `status=push_failed`, `commitCreated=true`, the local `commitSha`, `pushAttempted=true`, and `pushSucceeded=false`. `--fail-after-commit` triggers only after the commit and any requested push succeed; no-change runs remain normal success and neither push nor deliberate failure is attempted.

`repositoryChanges` reports `status`, exact path counts, bounded sorted `changedPaths` and `unexpectedTrackedPaths`, truncation flags, commit creation/SHA, push attempt/success, fail-after-commit request/trigger state, and a scoped error. Each path list is limited to 200 entries, 64 KiB total text, and 4 KiB per displayed path while the count remains exact. Final JSON remains the sole stdout document; DAG and repository progress stays on stderr. Final statuses are `precondition_failed`, `skipped_dag_failed`, `no_changes`, `repository_state_changed`, `unexpected_tracked_changes`, `commit_failed`, `committed`, `pushed`, `push_failed`, and `failed_after_commit`.

`watch` connects to the per-worktree daemon, runs an initial watch-mode cycle, then keeps polling for changes and reruns only the affected downstream slice. In attached JSON mode it emits the typed event stream line-by-line.

Watch file matching is driven by adapter task inputs. Changed files directly affect tasks whose `Inputs.Files`, `Inputs.Dirs`, `Inputs.Globs`, or `Inputs.Filtered` paths match the changed paths, then the engine cascades through downstream tasks that are eligible to rerun in watch mode.

The watcher is scoped to declared inputs in the selected target closure plus Devflow's flush sync directory. This keeps idle watch daemons from recursively polling unrelated dependency trees such as `node_modules`. If a project truly needs to watch a normally ignored directory, declare it as an input path.

Watch cascades respect dependency barriers. If an intermediate task in the affected slice is not allowed to run in watch mode, downstream tasks past that intermediate are not run in that cycle.

`graph affected --files a,b --explain --json` reports why changed files do or do not affect tasks. Explanations include direct file matches, directory matches, glob matches, filtered matches, ignored paths, and unmatched files. This is the primary debugging tool for generated-output watch loops.

`validate` hardens finite task graphs without changing the real worktree:

```bash
devflow validate build --mode artifacts --json
devflow validate build --mode orders --max-orders 1000 --json
devflow validate build --mode all --details issues --max-listed-paths 200 --json
```

`--mode all` is the default. `permutations` is accepted as an alias for `orders`, and `--max-permutations` is an alias for `--max-orders`.

`--details summary|issues|full` controls response volume. JSON defaults to `issues`, text output defaults to `summary`, and embedded Go API callers retain the historical exhaustive zero-value behavior unless they set `validation.Request.Details`. `summary` keeps pass/fail, exact counts, timings, byte/resource metrics, and phase data. `issues` adds bounded actionable samples but removes exhaustive successful-path arrays. `full` explicitly requests the legacy exhaustive arrays. `--max-listed-paths` defaults to 200 per issue category; all listed issue/path/log text also shares a 512 KiB default bound, so unusually long paths cannot bypass the count limit.

Artifact mode resets a disposable sandbox before each task, copies only the task's declared worktree inputs plus declared outputs from transitive dependencies, executes the task with caches and stamps bypassed, and reports:

- the actual input files and dependency-output files materialized
- the declared output paths that exist after the task
- final observed file writes
- writes outside declared outputs
- declared outputs that are missing or have the wrong file/directory type
- a capped task log when execution fails

This is a sufficiency check: a successful task proves it can run with the declared worktree inputs and dependency outputs for that execution. It does not prove that all declared inputs are necessary.

Order mode enumerates every dependency-respecting topological order and runs each order one task at a time in the same logical, freshly reset sandbox. Declared outputs are removed from the initial source copy unless they are also in-place inputs of their producing task. Every order must succeed, produce every declared output, and produce the same final declared-artifact snapshot. The result includes each order, its output digest, its failed task/error when applicable, and paths that differ from the first successful order.

The default `--max-orders` is 1000. Devflow first enumerates up to the bound. When more valid orders exist, JSON returns `orders.complete=false`, `orders.runs=[]`, and an `order_limit_exceeded` issue; the command exits non-zero instead of validating only a sample. Raise the bound explicitly when exhaustive execution is intentional.

Both modes are direct and finite. Service/debug-service closures, overlapping output ownership, absolute/escaping artifact paths, `.git`/`.devflow` declarations, and worktree-root outputs fail preflight. The disposable sandbox isolates ordinary worktree-relative reads and writes, but it cannot isolate task-defined databases, network calls, global caches, absolute paths, or unregistered background processes. Select a finite, side-effect-safe target. Git metadata is deliberately not copied.

`validate --json` returns `ValidationResult` with `project`, `target`, `worktree`, normalized `mode`, `details`, exact counts, `samples`, `truncated`, `metrics`, optional `resourceFailure`, optional `artifacts`/`orders`, and structured issues. Validation findings still emit the complete JSON result and then exit non-zero. Metrics distinguish logical temporary bytes from `temporaryPhysicalBytesCurrent`/`temporaryPhysicalBytesPeak`; `temporaryPhysicalBytesMeasured` is true when Linux/macOS allocation-block data was available and false on the Windows fallback. The default limits are 5,000,000 cumulative files, 20 GiB cumulative logical bytes, 20 GiB validation-specific temporary logical bytes, and a 1 GiB disk safety reserve. They can be changed with `--max-files`, `--max-bytes`, `--max-temporary-bytes`, and `--disk-reserve-bytes`. The budget spans every phase and a failure reports its phase, resource, observed usage, limit, available bytes, reserve, and path before writable cleanup.

Every applicable validation phase emits an immediate start and completion event plus throttled changing counters (at most about once per second) through stderr: preparing, copying, projecting, running, capturing, analyzing, archiving, and cleanup. The event includes elapsed time, files/logical bytes processed, current/peak/remaining temporary bytes, and issue count. Under `--json`, stdout remains exactly one final JSON document. Tasks see `Runtime.Mode` as `validation`, `DEVFLOW_VALIDATION=1`, and `DEVFLOW_VALIDATION_MODE=artifacts|orders`.

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

`flush --json` returns `FlushResult` with the request ID, instance ID, worktree, project, target, mode, whether a daemon watch loop was started, sync/health success, node states, service health, and structured issues. The command exits non-zero when `success=false`, including timeout and health-check failures. Low-level watch-start, request-write, sync-write, and acknowledgement-read failures retain their daemon error as a phase-specific issue instead of returning an empty result; the CLI also adds a `daemon_error` issue when an older daemon returns an unstructured failed response.

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

`upgrade --version v0.1.2` installs that specific tag. `upgrade --direct` sets `GOPROXY=direct` for testing freshly pushed commits before the public Go proxy catches up. Upgrade emits immediate start/finish progress and streams the underlying `go install` stdout/stderr instead of buffering it until exit. In text mode the child keeps its stdout/stderr destinations. With `upgrade --json`, live progress and combined child output go to stderr while stdout remains one final JSON document containing the command, package, version target, success flag, duration, and captured `output`. It exits non-zero when the underlying `go install` fails. In text mode, `upgrade` warns when `go install` writes a binary somewhere other than the `devflow` command currently found on `PATH`.

`docs setup` prints the setup/pipeline user docs bundle. `docs development` prints the day-to-day CLI/TUI/operator user docs bundle.

Bare `docs` is intentionally a usage error so agents and users do not accidentally pull both context lanes into one prompt. The docs commands are projectless, have no flags, have no JSON mode, and do not print contributor docs.

`restart` connects to the daemon. A service restart is handled by the active engine that owns the service handle: it stops only the planned service set, preserves unrelated services, assigns a new process generation, waits through the task readiness probe, and reports success only after a different ready identity exists. Repeated requests are serialized. A failed or stopped service can be started again while its watch supervisor remains active. `restart --preview` returns the same `LifecyclePlan` without changing execution state. Non-service restart slices retain their finite attached execution behavior.

`stop` is daemon-backed; if no daemon is running, it may start a short-lived daemon to reconcile persisted runtime state. `stop --task` is genuinely task-scoped: the active engine stops the named service and any planned active service dependents without canceling the active run or unrelated services. An already-stopped known task succeeds idempotently with an empty stopped set; an unknown task fails before changing processes. `stop --preview` returns the plan without mutation and never shuts down the daemon. With `--all`, DevFlow snapshots the live stop scope before cancellation, then reports only confirmed active services, legacy supervisor/executor processes, a running managed database, and daemon cleanup. A partial failure retains confirmed `stopped` entries and adds per-resource `issues`; repeated cleanup does not claim absent resources.

Lifecycle JSON is additive and shared across CLI/TUI daemon actions. `LifecyclePlan` contains `requestedAction`, selected task/target, `tasksToInvalidate`, `processesToStop`, `tasksToExecute`, `servicesToPreserve`, `servicesToRestart`, and `confirmationRecommended`. `LifecycleResult` adds exact `affected`, confirmed `stopped`, and `restarted` sets plus old/new PID and generation identities, readiness, and optional `{resource,reason}` `issues`. A restart from an already-stopped service has no previous identity and an empty `stopped` set. Existing top-level stop/run fields remain present. Use:

```bash
devflow restart backend_debug --preview --json
devflow restart backend_debug --json
devflow stop --task backend_debug --preview --json
devflow stop --task backend_debug --json
```

`doctor` supports `--target <target>` and `--strict`. Without a target it checks the full adapter required CLI catalog and project/task required-env metadata. With a target it resolves the target or task name and checks only `RequiredCLIs` and `RequiredEnv` attached to that target and its task closure, plus project-wide required env. JSON includes `project`, `target`, `cliScope`, `checksPassed`, and `requiredEnv` entries with `name`, `set`, and the detected source. Normal doctor remains report-only; `--strict` emits the same complete text/JSON result and exits nonzero when any check fails.

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

`NodeStatus.pid` is a host-process identifier, not a universal service identity. `generation` is the monotonic engine-owned service identity and also works for PID-less handles; `attempt` exposes the corresponding attempt number. `ready` is set only after the service readiness callback succeeds. Process-backed services report a positive PID. Engine-managed resources such as the managed Postgres container report PID `0` while running; their liveness is held by the daemon's registered service handle and verified by `flush`, and their output still uses the normal task log/typed log-event surfaces. Detached start JSON always emits boolean `accepted`, `supervisorStarted`, and `ready`, plus the response-time `state`; use status/flush as the continuing health gate.

Task states now distinguish:
- `pending`, `starting`, `running`, `ready`, and `restarting`
- `cached` and `done`
- `failed`: the task itself failed
- `blocked`: a downstream task could not run and `lastError` identifies the failed dependency
- `degraded`: a service remains present but lifecycle control could not complete cleanly
- `migration_needed`: the task intentionally blocked because a database migration must be authored before downstream work can run
- `canceled`: the task was interrupted because another task failed or the run was canceled
- `stopped` and `dirty`

`logs` supports task logs as before and also accepts `supervisor` to read the daemon/supervisor log directly.

Task log files now represent the current run attempt for that task. The engine truncates the log at task-attempt start before adapter code can emit progress, and subprocess output appends within that attempt. Older successful, failed, or canceled output must not stay mixed into a newer running attempt. Task, daemon, and event-stream logs are owner-only (`0600`) on Unix-like systems.

`tui` now opens a live operator console connected to the per-worktree daemon. Without `--instance`, `devflow tui` follows the same default launch path as bare `devflow`: resolve the default target, ensure the per-worktree daemon is running it in watch mode, wait for a matching non-empty status snapshot, then render. With `--instance`, `tui` is attach-only and does not start or retarget work.

The operator console includes:
- instance/runtime header
- live task list with selection
- selected-task metadata
- a bounded live tail of the selected task log; running logs open at the tail in `FOLLOWING`, upward/Page Up scrolling switches to `PAUSED`, End or `f` resumes, and `o` loads older retained lines up to a fixed bound
- toggle to the daemon/supervisor log
- `d` toggles a database/Prisma panel with managed Postgres identity, persisted flavor (`postgres` or `postgis`), the selected PostgreSQL major when configured, configured/automatic image selection, and recent cached Prisma migration-prefix snapshots; `F2` is a backup key
- the database/Prisma panel flags schema/migration drift and `m` asks for a migration name, then sends a daemon action with kind `devflow.database.migration.create` through the daemon-owned engine and relaunches the previously detached target; `F4` is a backup key
- while the TUI creates a Prisma migration, the footer status reports target/task state and the latest task output line
- global shortcuts are disabled while text-input popups are focused, so migration names can contain normal letters
- stable graph/topological task order; state changes never move rows, while `a` explicitly toggles an attention-only view
- distinct monochrome-readable badges for waiting, starting, running, ready, restarting, cached, done, failed, canceled, blocked, stopped, degraded, and dirty states, with concise failure/block reasons
- `i` immediately invalidates and reruns the selected task scope without a confirmation modal; the daemon still calculates and applies the scoped lifecycle action
- `t` opens a real target chooser, previews stop/execute/preserve/start scope, then retargets only after confirmation
- `?` opens contextual help; Tab changes task/log focus, focused panes have distinct titles/borders, and popup/input footers advertise only valid keys
- responsive layouts place the task selector left of the log workspace at 120+ columns with at least 24 rows, retain the stacked compact layout below that breakpoint, preserve a selectable task row before hiding the optional log pane, and show a deliberate too-small fallback below 40x10
- popup confirm and text prompts for interactive tasks that emit `interaction_requested` events
- primary live refresh from the daemon event subscription, with the persisted event stream at `.devflow/state/instances/<instance-id>/events.jsonl` as fallback

Daemon ownership is session-scoped. If `devflow tui` or bare `devflow` has to start the daemon for that TUI session, quitting the TUI sends `stop --all` through the daemon so services, managed databases, and the daemon exit together. If the daemon already existed before the TUI connected, quitting closes only the UI and, after terminal restoration, prints the exact status and stop commands for the still-active instance.

Interactive prompt answers are written back through the instance interaction directory, so detached runs can still receive operator input from the TUI.

Implemented `tui` flags include:
- `--worktree`
- `--instance`

`cache status` lists entries for the selected project cache namespace, `cache invalidate` removes entries for that namespace globally or per task, and `cache gc` keeps only the newest N entries per task in that namespace. Task cache storage is physically global under the OS user cache directory, but entries are grouped by project namespace.

`cache key --target <target>` prints the aggregate key for all cacheable/stamped tasks in the selected closure. Its JSON form returns `project`, `target`, `instanceId`, `namespace`, `key`, and `taskKeys`; each task item includes its real task key and whether it is a local stamp. It computes keys with the normal resolved instance environment and finalizers but does not execute target tasks.

Use the explicit manifest handoff when that preflight includes expensive semantic fingerprint callbacks:

```bash
devflow cache key --target ci-backend --manifest-out "$RUNNER_TEMP/devflow-cache-manifest.json" --json
devflow run ci-backend --cache-key-manifest "$RUNNER_TEMP/devflow-cache-manifest.json" --ci --json
```

The first JSON still contains the exact aggregate `key` for the external cache action and adds `manifestPath`. Devflow creates the manifest atomically with owner-only permissions. The second command validates its integrity, schema/build, 15-minute expiry, project/namespace/worktree/target, graph/configuration, task signatures, and hashed environment bindings. An explicitly supplied invalid manifest is a structured run failure with `cacheKeyManifest.validated=false` and a redacted `cacheKeyManifest.error`; Devflow does not silently execute the costly callbacks. The run rehashes local inputs and dependency keys, reuses only the semantic component snapshot, and reports reuse under `cacheKeyManifest` plus per-node `cache.manifestComponents`, `manifestValidationMs`, and `localInputsChangedFromManifest`.

`cache path` prints the selected namespace's physical entry path. Its JSON form returns `project`, `namespace`, `cacheRoot`, and `namespacePath`, which is the supported surface for GitHub Actions or other CI cache configuration.
