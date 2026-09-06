# CLI

## JSON errors and cancellation

Finite JSON commands emit one result on stdout, including argument, adapter bootstrap, resolution, admission, transport and execution failures. Failure results have `success: false` and a shared `error` object:

```json
{"success":false,"error":{"code":"unknown_target","phase":"resolution","message":"unknown target \"missing\""}}
```

Available command-specific evidence stays at the top level: a failed run retains `nodes`, `failedNode`, log paths, excerpts, cache timings and repository changes; validation retains its issues and metrics; flush retains synchronization and health evidence. `api.CommandError` is the common error type in run, lifecycle, upgrade, flush and daemon results. Nested task diagnostics and event errors remain text. There are no string-error readers or top-level code aliases.

| Code | Phase | Meaning |
| --- | --- | --- |
| `invalid_arguments` | `parsing` | Unknown flag/subcommand, missing value, extra positional argument or invalid option combination |
| `adapter_not_found`, `adapter_source_invalid` | `bootstrap` | Missing marker or invalid adapter source file |
| `adapter_compile_failed`, `bootstrap_failed` | `bootstrap` | Go compilation failure or another local build/launch failure |
| `unknown_project`, `ambiguous_project`, `unknown_target`, `invalid_graph`, `unknown_instance`, `invalid_worktree` | `resolution` | Project, graph, target or instance could not be resolved |
| `unknown_run`, `run_expired` | `resolution` | The run was never issued here or its evidence was pruned |
| `interaction_required` | `execution` | Headless fail policy stopped a task that requested input |
| `invalid_prompt`, `unknown_prompt`, `prompt_mismatch`, `invalid_prompt_answer`, `prompt_not_pending` | `interaction` | Prompt identity, type or lifecycle rejected the request |
| `run_mismatch` | `admission` | A supplied run identity does not match the execution selection |
| `run_not_active`, `evidence_unavailable`, `evidence_write_failed`, `retention_failed` | `execution` | A terminal run cannot be cancelled, or retained evidence could not be read |
| `resource_conflict` | `admission` | Worktree ownership prevented execution; inspect `resourceConflict` |
| `daemon_unavailable` | `transport` | Daemon startup or socket communication failed |
| `task_failed`, `validation_failed`, `flush_failed`, `doctor_failed`, `repository_failed`, `upgrade_failed`, `log_read_failed` | `execution` | Inspect the command's retained evidence |
| `operation_cancelled`, `deadline_exceeded` | phase where interrupted | Context cancellation or deadline interrupted the operation |
| `operation_failed` | `execution` | Unclassified operational failure; inspect the message and any result evidence |

Codes are assigned from error types and source boundaries, never inferred from prose. The outer entrypoint discovers the selected command's actual flag definitions before bootstrap. `--json`/`--json=true` are recognized around malformed flags and on either side of positional arguments; a value such as `--project --json`, or a token after `--`, does not enable JSON. `--json=false` opts out. Ordinary text errors are reported once on stderr. JSON errors already presented by the local binary are not printed again by bootstrap entrypoints.

Logs and attached watch use JSONL: each record occupies one line, including watch start metadata, events and a terminal error when the stream fails. Watch emits no plain banner in JSON mode. A failed output writer returns an error without attempting another document on that writer. Progress remains independent stderr text for finite runs and validation; progress verbosity controls are a later item.

`App.Context`, Ctrl+C and termination signals propagate to direct CI, validation, bootstrap compilation and local lock waits, repository repair, CLI installers and client waits. A canceled adapter build does not publish its temporary binary/key. Finite subprocess and Git cancellation stops their process trees. Unix bootstrap transfers into the local binary; Windows owns a child process and preserves its exit/result ownership. Cancelling a log reader, watch subscription or flush wait leaves the daemon's development execution running. Execution deadlines and `runs cancel <run-id>` address the operation itself; cancellation of a client wait alone does not cancel server work. A cancel response acknowledges the request; `runs show` supplies the eventual cleanup and terminal result.

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
- `devflow runs list|show|cancel`
- `devflow prompts list|respond`
- `devflow instances`
- `devflow doctor`
- `devflow clis`
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

Only one execution may own a worktree at a time. Direct `run --ci` remains daemon-independent, but returns a nonzero `resource_conflict` if a watcher or another run owns that worktree. Rejected execution leaves its owner's task status, task logs, environment and outputs intact. `cache key` (which prepares instance configuration) and direct `cache invalidate` use the same admission boundary. Status/graph/log inspection remains available; existing parallel tasks within one DAG are unaffected.

Ownership failures add `error.code: "resource_conflict"` and `resourceConflict` to JSON without replacing existing successful response shapes. `resourceConflict` identifies the worktree and, when available, owner PID, target, mode and kind; `recoveryRequired: true` distinguishes abandoned/incompletely cleaned execution. CI retains its failed `RunResult` shape with empty task results when rejected before execution. Daemon mutations propagate the same typed conflict over the socket and CLI.

Use a separate worktree containing the changes to run CI while keeping development active, or explicitly stop the development execution first. A successful flush does not grant concurrent CI admission. Stop timeouts reject replacement and preserve ownership. `stop --all` can reconcile known abandoned resources, but reports a conflict when it cannot establish that resources stopped; it never kills a competing live direct-CI owner to obtain admission.

Service lifecycle contract:
- attached non-CI `devflow run <target>` connects to the per-worktree daemon and waits for service readiness and execution completion. Interrupting this client ends its wait; daemon-owned services continue until `runs cancel <run-id>`, an applicable `stop` command, or execution termination
- if a service exits during attached `run`, the command returns a service-exited error
- `devflow run <target> --ci --json` is finite and deliberately bypasses the daemon; service tasks are started, readiness is checked, services are stopped, and status records those services as `stopped`
- in that CI/JSON mode, task state, cache hit/miss, and task-log progress streams to stderr while stdout remains exactly one final JSON document
- `devflow run <target> --detach --json` returns after asking the daemon to launch the target; it is not a health/readiness gate. `accepted`, `daemonStarted`, `ready`, and `state` distinguish daemon acceptance, daemon startup, and the response-time `starting|ready|failed|degraded` target snapshot. `daemonPid` identifies the daemon; the result also includes `runId`, `instanceId`, `target`, `mode`, and `logPath`.
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
- `--headless fail|wait` (default `fail`)
- `--timeout <duration>` (operation deadline; default `0` means no overall deadline)
- `--cache-key-manifest` (finite `--ci` runs only)
- `--commit-changes` (finite `--ci` runs only)
- repeated `--commit-path <git-pathspec>`
- `--commit-message <message>`
- `--push`
- `--fail-after-commit`
- `--pedantic` (treat CRLF/LF-only changes as commit-worthy)

The final `RunResult` includes `runId`, structured `error`, failed-node name and log path, an optional bounded terminal tail, `failureExcerpts`, cache hit/miss lists, optional `repositoryChanges`, and the final run snapshot of every selected node. Downstream work skipped after a dependency failure is `blocked` with the dependency in `lastError`; unrelated interrupted work is `canceled`. Each node includes `durationMs`; cacheable nodes also include cache outcome plus key/read/write/manifest/total timing. `failureExcerpts` scans the log as a stream and recognizes Go `--- FAIL:` blocks and `*_test.go:line:` diagnostics, panic/fatal output, compiler keywords or `file.go:line:column:` locations, `AssertionError`, conventional error/failed-test summaries, and process-failure markers. It keeps up to five context lines before and 30 after, merges nearby windows, removes overlap between adjacent windows, and is capped at five windows, 200 total lines, 64 KiB total text, and 8 KiB per line. A window is omitted if the aggregate cap cannot retain its triggering marker. For an early service exit with no recognized marker, one `process-exit-tail` window keeps the last 12 meaningful bounded lines. Excerpts and the terminal tail use the same environment-secret/PostgreSQL-URL redaction. Empty logs still produce `[]`.

Engine configuration failures, including cached tasks without output declarations, also produce one failed `RunResult` for `run --ci --json`. They return before instance configuration or task execution; `nodes` is empty, and `repositoryChanges` is present only when repository repair was requested.

### Retained runs, prompts and scoped cancellation

Every admitted operation has one `runId`, including direct CI, daemon runs, watch sessions and foreground actions. Every task attempt has a new `attemptId`; task state and log events carry both identities. Watch reruns keep the watch run ID and allocate new attempts. `runId` identifies evidence, while the worktree execution lease still determines whether execution may overlap.

```bash
devflow runs list --json
devflow runs show <run-id> --json
devflow runs cancel <run-id> --json
devflow logs <task> --run <run-id> --attempt <attempt-id> --tail 100 --json
```

These commands, `prompts`, and ordinary `logs` inspection do not compile or load the adapter. They accept `--worktree` or `--instance`, so retained evidence remains accessible after adapter compilation breaks. `runs list --json` returns `instanceId` and run summaries; `runs show --json` returns the retained record plus `prompts`. A run record includes project/target/mode, timestamps, deadline, graph digest, compiled adapter digest, attempts and the available final `result`. Each attempt records its task, identity, timestamps, outcome, log path, failure details, and the cache/input key when computed. `executed` distinguishes callback execution from cache/stamp skips. Group nodes and tasks never started by the scheduler have no attempt record. A successful historical result describes the inputs consumed in that run; it does not certify later edits.

Run states are `queued`, `running`, `waiting`, `succeeded`, `failed` and `canceled`. A run remains `waiting` while any of its parallel tasks has a pending prompt. `runs cancel` returns `accepted: true` after recording a request for that exact run. The owner stops its resources and writes the terminal result; canceling an old run never targets a replacement development session. Unknown or pruned IDs report `unknown_run` or `run_expired`; a terminal run rejects cancellation with `run_not_active`. Status remains the current development view, while `runs show` supplies historical results. `runs list/show` include `ownerAlive` for nonterminal records with a recorded owner PID. When it is `false`, the interrupted record can remain nonterminal; cancellation acceptance proves neither cleanup nor that a final result will arrive.

Run evidence and attempt logs live under `.devflow/state/instances/<instance-id>/runs/<run-id>/`. Records are replaced atomically and files are owner-only on Unix-like systems. Retention targets 100 completed records, seven days and 64 MiB of completed evidence, oldest first. Completed-run pruning runs before the current terminal result is committed, so retention failures are included in that immutable result (`retention_failed`). The newly completed result can put evidence over the count/age/byte thresholds until the next pruning pass.

Pruning first renames a completed run into a hidden retirement directory; a blocked rename preserves its published record, and failed physical deletion leaves hidden data retried by later pruning. Retired IDs return `run_expired`; following a retired attempt stops with that error. Active/interrupted records and pending physical cleanup can exceed the retention budget.

`run`, `watch`, `action run`, and the migration shortcut accept `--headless fail|wait` and `--timeout`. Headless defaults to `fail`: a declared prompt stops the task with `interaction_required`, closes the diagnostic prompt, and cleans up owned resources. Supply known action inputs first. Explicit `--headless wait` leaves a discoverable waiting operation; each prompt expires after five minutes or the earlier operation/context deadline. A nonzero operation timeout must be at least 1 ms and covers execution, including daemon work after an observer disconnect. It is distinct from the existing `flush --timeout` wait budget. No headless mode automatically confirms choices.

```bash
devflow run verify --ci --headless wait --timeout 10m --json
devflow prompts list --run <run-id> --json
devflow prompts respond <prompt-id> --run <run-id> --task <task> --attempt <attempt-id> --confirm false --json
devflow prompts respond <prompt-id> --run <run-id> --task <task> --attempt <attempt-id> --text 'migration-name' --json
devflow prompts respond <prompt-id> --run <run-id> --task <task> --attempt <attempt-id> --stdin --json
```

Use another CLI while the first command waits. Prompt metadata contains `id`, `runId`, `task`, `attemptId`, `kind`, `message`, optional `secret`, `state`, `createdAt` and `deadline`. States are `pending`, `answered`, `cancelled` and `expired`. Inspection reconstructs deadline/cancellation state without changing retained prompt metadata. Responses require exactly one boolean `--confirm true|false`, text `--text`, or text `--stdin`; false and empty text are valid answers. Stdin reads UTF-8 through EOF, removes one trailing LF or CRLF, and is limited to 64 KiB. Kind mismatches return `invalid_prompt_answer`; identity mismatches return `prompt_mismatch`; duplicate, cancelled, expired or finished requests return `prompt_not_pending`. Prompt identity is unique across parallel tasks and retries. Only the latest active attempt of a task may create, answer or consume a prompt; a stopped or replaced service attempt is closed even while its old process reader is still unwinding. An answer accepted just before that transition is removed without delivery. Response admission shares a cross-process lock with run cancellation, finalization and pruning.

Response JSON acknowledges identities and `accepted`; it never echoes the value. Values travel through temporary owner-only answer files that are removed on consumption or cleanup, separate from retained prompt metadata, events and results. Marking an adapter prompt `Secret` also masks TUI input. Once that response is sent, Devflow hides subsequent subprocess output and emits `[output hidden after secret response]`, while continuing to recognize later prompts. This prevents child echoes from entering task logs/events; adapters must still avoid printing secrets themselves or including them in returned errors.

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

`--commit-changes` requires `--ci`, at least one repeated `--commit-path`, and a non-empty `--commit-message`. `--push`, `--fail-after-commit`, `--pedantic`, `--commit-path`, and `--commit-message` are invalid without `--commit-changes`. The selected Devflow worktree must be the Git worktree root, have an existing `HEAD`, and be completely clean before the engine starts, including tracked, staged, and untracked status entries.

After the complete DAG succeeds, Devflow verifies that `HEAD` and its branch/detached state still match the preflight baseline. Git itself interprets every supplied pathspec, including pathspec magic. Devflow lists candidate tracked and untracked changes only through those pathspecs and separately scans tracked status across the repository. By default, tracked changes whose staged and unstaged Git diffs become empty under `--ignore-cr-at-eol` are treated as CRLF/LF-only churn: they do not count as permitted changes or unexpected tracked changes, are removed from the index if the DAG staged them, and remain modified but unstaged in the worktree. `--pedantic` disables that exception and restores byte-sensitive behavior. Other tracked changes outside the permitted set are rejected. Untracked paths outside the supplied pathspecs are not staged or committed. A failed DAG skips all post-run Git inspection, staging, commit, and push work.

When permitted material changes exist, Devflow runs Git directly without a command shell, stages only the exact Git-reported paths remaining after line-ending filtering, and verifies that the staged, ignored, and unexpected sets stay stable. It writes that exact index tree, creates one commit object, and advances `HEAD` with an old-SHA compare-and-swap. This exact-tree plumbing path does not invoke normal commit hooks or commit signing. A staging/commit failure restores the previously clean index while retaining worktree edits. If neither configured `user.name`/`user.email` nor complete role-specific Git identity environment variables are available, the new author and committer identities are derived from the corresponding identities on `HEAD`.

`--push` invokes the configured plain `git push` only after the local commit exists. A failed push does not roll back the commit: the final result is nonzero with `status=push_failed`, `commitCreated=true`, the local `commitSha`, `pushAttempted=true`, and `pushSucceeded=false`. `--fail-after-commit` triggers only after the commit and any requested push succeed; no-change runs remain normal success and neither push nor deliberate failure is attempted.

`repositoryChanges` reports `status`, `pedantic`, exact path counts, bounded sorted `changedPaths`, `ignoredLineEndingPaths`, and `unexpectedTrackedPaths`, truncation flags, commit creation/SHA, push attempt/success, fail-after-commit request/trigger state, and a scoped error. Each path list is limited to 200 entries, 64 KiB total text, and 4 KiB per displayed path while the count remains exact. Final JSON remains the sole stdout document; DAG and repository progress stays on stderr. Final statuses are `precondition_failed`, `skipped_dag_failed`, `no_changes`, `repository_state_changed`, `unexpected_tracked_changes`, `commit_failed`, `committed`, `pushed`, `push_failed`, and `failed_after_commit`.

`watch` connects to the per-worktree daemon, captures its input baseline before the initial watch-mode cycle, then reconciles changes that arrived during execution and reruns only the affected downstream slice. In attached JSON mode it emits the typed event stream line-by-line.

Watch file matching is driven by adapter task inputs. Changed files directly affect tasks whose `Inputs.Paths`, `Inputs.Files`, `Inputs.Dirs`, `Inputs.Globs`, or `Inputs.Filtered` paths match the changed paths, then the engine cascades through downstream tasks that are eligible to rerun in watch mode.

The watcher is scoped to declared inputs in the selected target closure plus Devflow's flush sync directory. Root-level globs such as `*.go` or `**/*.go`, and an explicit `.` input, scan from the worktree root while retaining default ignores. This keeps idle watch daemons from recursively polling unrelated dependency trees such as `node_modules`. If a project truly needs to watch a normally ignored directory, declare it as an input path.

Watch cascades respect dependency barriers. If an intermediate task in the affected slice is not allowed to run in watch mode, downstream tasks past that intermediate are not run in that cycle.

`graph affected --files a,b --explain --json` reports why changed files do or do not affect tasks. Explanations include direct file matches, directory matches, glob matches, filtered matches, ignored paths, and unmatched files. This is the primary debugging tool for generated-output watch loops.

`validate` hardens finite task graphs without changing the real worktree:

```bash
devflow validate build --mode artifacts --json
devflow validate build --mode orders --max-orders 1000 --json
devflow validate build --mode all --details issues --max-listed-paths 200 --json
```

`--mode all` is the default. Use `orders` and `--max-orders` to select and bound exhaustive order validation.

`--details summary|issues|full` controls response volume. JSON and omitted `validation.Request.Details` both default to `issues`; text output defaults to `summary`. `summary` keeps pass/fail, exact counts, timings, byte/resource metrics, and phase data. `issues` adds bounded actionable samples but removes exhaustive successful-path arrays. `full` explicitly requests exhaustive arrays. `--max-listed-paths` defaults to 200 per issue category; all listed issue/path/log text also shares a 512 KiB default bound, so unusually long paths cannot bypass the count limit.

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

Both modes execute `BeforeRun` followed by optional `Run`, including tasks implemented only through a hook. A hook failure prevents `Run` and appears in the existing task/order error and captured log fields. Artifact snapshots include final hook writes, even on failure. Hook-provided runtime environment values reach that task's `Run` without changing the shared instance environment. Prompts fail immediately; service readiness and `AfterReady` do not run. If a finite task registers supervised services, validation rejects it, attempts to stop them, and includes any stop failure alongside the callback diagnostic.

`validate --json` returns `ValidationResult` with `project`, `target`, `worktree`, normalized `mode`, `details`, exact counts, `samples`, `truncated`, `metrics`, optional `resourceFailure`, optional `artifacts`/`orders`, and structured issues. Validation findings still emit the complete JSON result and then exit non-zero. Metrics distinguish logical temporary bytes from `temporaryPhysicalBytesCurrent`/`temporaryPhysicalBytesPeak`; `temporaryPhysicalBytesMeasured` is true when Linux/macOS allocation-block data was available and false on the Windows fallback. The default limits are 5,000,000 cumulative files, 20 GiB cumulative logical bytes, 20 GiB validation-specific temporary logical bytes, and a 1 GiB disk safety reserve. They can be changed with `--max-files`, `--max-bytes`, `--max-temporary-bytes`, and `--disk-reserve-bytes`. The budget spans every phase and a failure reports its phase, resource, observed usage, limit, available bytes, reserve, and path before writable cleanup.

Every applicable validation phase emits an immediate start and completion event plus throttled changing counters (at most about once per second) through stderr: preparing, copying, projecting, running, capturing, analyzing, archiving, and cleanup. The event includes elapsed time, files/logical bytes processed, current/peak/remaining temporary bytes, and issue count. Under `--json`, stdout remains exactly one final JSON document. Tasks see `Runtime.Mode` as `validation`, `DEVFLOW_VALIDATION=1`, and `DEVFLOW_VALIDATION_MODE=artifacts|orders`.

`Inputs.Ignore` uses the same path-matching model for fingerprinting and watch matching:
- exact or glob matches use slash-normalized paths
- a pattern also suppresses descendants when the changed path has that pattern as a path prefix
- for directory inputs, ignore patterns are checked both root-relative and relative to the input directory
- for explicit file inputs, root-relative ignore patterns can suppress that file

For service restart policies, `RestartNever` blocks watch restarts, `RestartOnInputChange` follows the affected downstream slice, and `RestartAlways` restarts the service on any watch cycle that affects the selected target.

For watch-cycle events:
- `files` contains reconciled task-input changes after removing sync sentinels and immediate task-output writes
- `affectedTasks` is the directly affected task list derived from those file changes

`watch` also supports `--detach`.

`flush` is the readiness gate for detached watch workflows. It captures the selected project and target's active watch, waits for its observer baseline, and publishes a request plus sync sentinel. The engine reconciles queued, debounced, and newly observed changes after startup, rebuilds, and health probes before acknowledging the request. A replacement watch cannot satisfy the request, even when it selects the same target.

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
- without a positional target, a live daemon watch loop supplies its actual active target
- without a live watch loop, `inst.LastRun.Target` is reused when present
- otherwise the project preferred target is used

Daemon behavior:
- no daemon-owned watch loop: starts `devflow watch <target> --detach` through the daemon
- live daemon watch loop for the same project and target: reused after its observer baseline is ready
- live daemon watch loop for a different project: fails with `project_mismatch`
- live daemon watch loop for a different target: fails with `target_mismatch`
- live daemon non-watch work: fails with `non_watch_execution`
- captured watch stops or is replaced while waiting: fails with `watch_stopped`
- request context is canceled or reaches its deadline: fails with `canceled` or `timeout`; timeout sets `timedOut=true`

`flush --json` returns `FlushResult` with the request ID, instance ID, worktree, project, target, mode, whether a daemon watch loop was started, sync/health success, node states, service health, and structured issues. The command exits non-zero when `success=false`, including timeout and health-check failures. Low-level watch-start, request-write, sync-write, and acknowledgement-read failures retain their daemon error as a phase-specific issue instead of returning an empty result; the CLI adds a `daemon_error` issue when daemon communication fails or a response is missing its flush result.

`synced=true` only confirms that observation processing produced an acknowledgement. Require `success=true` for freshness and health. A changed task excluded by `RestartNever`, a warmup without `AllowInWatch`, or a dependency barrier produces `watch_restart_required` and `success=false`, even if its previous state is successful. The issue persists across flushes until the task executes successfully or the target is explicitly restarted. For a complete target restart, stop the existing execution and start its watch again.

The guarantee covers declared inputs visible to metadata-based polling through the final scan. It does not prove transient or metadata-preserving edits were observed, or execute checks outside the target. Scanner failures cancel watch execution and trigger normal cleanup. Generated-output changes are suppressed only when their current metadata matches the producer's completion record. Edits made after that producer finishes remain eligible for reruns, including edits to files the task also rewrites while downstream work is still running. Sibling source paths are not suppressed.

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

`upgrade --version v0.1.2` installs that specific tag. `upgrade --direct` sets `GOPROXY=direct` for testing freshly pushed commits before the public Go proxy catches up. After installation succeeds, upgrade clears the global task artifact cache at `<os.UserCacheDir()>/devflow/cache` so subsequent runs rebuild artifacts with the installed code. Failed installation leaves the cache intact. Cache cleanup failure returns an error even though the binary was installed. Run upgrades between executions: this global cleanup is not coordinated with active task-cache reads or writes. Upgrade does not migrate older APIs or worktree state.

Upgrade emits immediate start/finish progress and streams the underlying `go install` stdout/stderr instead of buffering it until exit. In text mode the child keeps its stdout/stderr destinations. With `upgrade --json`, live progress and combined child output go to stderr while stdout remains one final JSON document containing the command, package, version target, success flag, `cacheCleared`, duration, and captured `output`. It exits non-zero when installation or cache cleanup fails. In text mode, `upgrade` warns when `go install` writes a binary somewhere other than the `devflow` command currently found on `PATH`.

`docs setup` prints the setup/pipeline user docs bundle. `docs development` prints the day-to-day CLI/TUI/operator user docs bundle.

Bare `docs` is intentionally a usage error so agents and users do not accidentally pull both context lanes into one prompt. The docs commands are projectless, have no flags, have no JSON mode, and do not print contributor docs.

`restart` connects to the daemon. A service restart is handled by the active engine that owns the service handle: it stops only the planned service set, preserves unrelated services, assigns a new process generation, waits through the task readiness probe, and reports success only after a different ready identity exists. Repeated requests are serialized. A failed or stopped service can be started again while its daemon watch loop remains active. `restart --preview` returns the same `LifecyclePlan` without changing execution state. Non-service restart slices retain their finite attached execution behavior.

`stop` is daemon-backed; if no daemon is running, it may start a short-lived daemon to reconcile persisted runtime state. `stop --task` is task-scoped: the active engine stops the named service and any planned active service dependents without canceling the active run or unrelated services. An already-stopped known task succeeds idempotently with an empty stopped set; an unknown task fails before changing processes. `stop --preview` returns the plan without mutation and never shuts down the daemon. With `--all`, DevFlow snapshots the live stop scope before cancellation, then reports confirmed stops of engine-owned services, explicitly recorded process/status refs, a running managed database, and the daemon. A partial failure retains confirmed `stopped` entries and adds per-resource `issues`; repeated cleanup does not claim absent resources. Recovery never discovers processes by scanning old launcher commands or parsing logs.

Lifecycle JSON is additive and shared across CLI/TUI daemon actions. `LifecyclePlan` contains `requestedAction`, selected task/target, `tasksToInvalidate`, `processesToStop`, `tasksToExecute`, `servicesToPreserve`, `servicesToRestart`, and `confirmationRecommended`. `LifecycleResult` adds exact `affected`, confirmed `stopped`, and `restarted` sets plus old/new PID and generation identities, readiness, and optional `{resource,reason}` `issues`. A restart from an already-stopped service has no previous identity and an empty `stopped` set. Existing top-level stop/run fields remain present. Use:

```bash
devflow restart backend_debug --preview --json
devflow restart backend_debug --json
devflow stop --task backend_debug --preview --json
devflow stop --task backend_debug --json
```

`doctor` supports `--target <target>` and `--strict`. Without a target it checks the full adapter required CLI catalog and project/task required-env metadata. With a target it resolves the target or task name and checks only `RequiredCLIs` and `RequiredEnv` attached to that target and its task closure, plus project-wide required env. JSON includes `project`, `target`, `cliScope`, `checksPassed`, and `requiredEnv` entries with `name`, `set`, and the detected source. Normal doctor remains report-only; `--strict` emits the same complete text/JSON result and exits nonzero when any check fails.

`clis status` reports adapter-defined required CLIs, whether they are already installed, and whether a platform install script is available. `clis status --target <target>` uses the same CLI scope as target-scoped doctor. JSON includes `requiredCLIs`.

`clis install` runs adapter-defined install scripts only for missing required CLIs and then re-checks that each installed command is now available on `PATH`. `clis install --target <target>` installs only CLIs needed for that target closure.

`status` is read-only: it uses a live daemon when one is already running, otherwise it reads the persisted instance/status files without starting a daemon. It reports instance metadata in both text and JSON forms, including:
- worktree
- target, mode, active `runId` and `pendingPrompts`
- assigned ports
- sanitized DB details
- derived local URLs such as `backend`
- `daemon` metadata with PID, liveness, and log path when present
- per-node debug metadata for `debug_service` tasks, including host, port, port name, binary path, package, protocol, and a Go remote-attach shape

`NodeStatus.pid` is a host-process identifier, not a universal service identity. `generation` is the monotonic engine-owned service identity and also works for PID-less handles; `attempt` is the task-local attempt count; `runId` and `attemptId` are the durable execution identities, including for finite tasks. `ready` is set only after the service readiness callback succeeds. Process-backed services report a positive PID. Engine-managed resources such as the managed Postgres container report PID `0` while running; their liveness is held by the daemon's registered service handle and verified by `flush`, and their output still uses the normal task log/typed log-event surfaces. Detached start JSON always emits boolean `accepted`, `daemonStarted`, and `ready`, plus `daemonPid` and the response-time `state`; use status/flush as the continuing health gate.

Task states now distinguish:
- `pending`, `starting`, `running`, `ready`, and `restarting`
- `cached` and `done`
- `failed`: the task itself failed
- `blocked`: a downstream task could not run and `lastError` identifies the failed dependency
- `degraded`: a service remains present but lifecycle control could not complete cleanly
- `migration_needed`: the task intentionally blocked because a database migration must be authored before downstream work can run
- `canceled`: the task was interrupted because another task failed or the run was canceled
- `stopped` and `dirty`

`logs` accepts task names and the reserved sources `daemon` and `tui`. `logs daemon` reads the daemon log directly. `logs tui` reads `.devflow/logs/<instance-id>/tui.log`, including session boundaries, returned terminal errors, recovered panic stacks, and any Go fatal output duplicated while the TUI owned stderr. JSON mode uses JSON lines with `task: "daemon"` or `task: "tui"` for those sources.

`logs --tail N` selects the last N lines (default 50); zero streams the whole file and negative values are argument errors. Empty files produce no log records, and blank lines are preserved. Tail selection scans backward in fixed-size chunks, then the same consumed-byte cursor continues through `--follow`, so initial output is not replayed and appends during tail delivery are retained. Memory stays bounded independently of file size and requested line count. A line above 4 MiB fails explicitly with `log_read_failed`; CLI logs do not use the TUI's line truncation policy. Output-writer failures and command cancellation terminate reading.

Finite reads include a final unterminated line. Follow mode buffers it until a newline arrives, preserving characters split across writes. When polling detects replacement, shrinkage, or changed bytes at the cursor, it emits any remaining old partial line and reads the new file from its beginning. Follow closes file handles between 250 ms polls and tolerates a briefly absent replacement path; an initially missing log remains an error. A rewrite detected during a read fails with `log_read_failed` so mixed or incomplete evidence cannot appear as a successful finite read. This observes current files rather than retaining history: content overwritten between polls cannot be recovered, and a truncate-and-regrow with identical bytes in the bounded cursor check can be indistinguishable from an append. Immutable task attempts preserve older task output. Resumable cursors and following across attempts remain separate work.

Task logs are created once at `runs/<run-id>/attempts/<attempt-id>.log` beneath the instance state directory, then appended only within that attempt. A new attempt never truncates an older log. `logs <task>` selects the current status attempt; `--run` selects the latest retained attempt of that task within the chosen run, and `--attempt` selects one exact attempt and requires `--run`. The task and attempt must match. JSON task log records include `task`, `runId`, `attemptId` and `line`. Selection is pinned when the command starts: `--follow` follows that one attempt, including when selected from current status. Reissue the command after a watch rerun or restart to select its new attempt. Task, daemon, TUI, and event-stream logs are owner-only (`0600`) on Unix-like systems.

`tui` now opens a live operator console connected to the per-worktree daemon. Without `--instance`, `devflow tui` follows the same default launch path as bare `devflow`: resolve the default target, ensure the per-worktree daemon is running it in watch mode, wait for a matching non-empty status snapshot, then render. With `--instance`, `tui` is attach-only and does not start or retarget work.

The operator console includes:
- instance/runtime header
- live task list with selection
- selected-task metadata
- a bounded live tail of the selected task log; running logs open at the tail in `FOLLOWING`, upward/Page Up scrolling switches to `PAUSED`, End or `f` resumes, and `o` loads older retained lines up to a fixed bound
- toggle to the daemon log
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

TUI startup creates/appends the owner-only per-instance `tui.log` before terminal initialization. Panics on the application goroutine are caught after tview finalizes the screen; Devflow records the panic and stack and returns a concise error containing the log path. Every Devflow-owned TUI background worker uses the same recovery boundary, records the first panic, and stops the application so the terminal can be restored. Go fatal output and panics in dependency-owned goroutines, which cannot be recovered by those boundaries, are duplicated directly to the same file by the runtime crash-output hook. Normal application errors are also retained there.

TUI executions explicitly select headless waiting. The TUI restores pending prompt metadata when reconnecting, keeps an open dialog while parallel requests queue, and responds through the same run/task/attempt-bound protocol as `prompts respond`. It never echoes submitted answer values into its status log.

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
