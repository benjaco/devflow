# Agent Integration

Devflow is designed so humans and agents use the same execution surface:
- operational CLI commands have stable JSON output
- instance and task state are persisted
- results and task logs are addressable by instance, run and attempt
- one per-worktree daemon owns mutable dev/watch/operator work and publishes a typed event stream for live consumers
- `run --ci` is the exception: it stays direct and finite for CI-style validation

Executions are exclusive within a worktree. While a watcher owns it, `run --ci --json` returns `error.code: "resource_conflict"` with the owner's target/PID and leaves the development execution intact. Run independent verification in a separate worktree containing the actual edits and relevant untracked files, or explicitly stop development first. Flushing does not make concurrent CI safe. Worktrees do not automatically isolate a shared external database.

`resourceConflict.recoveryRequired` means a prior execution ended without confirmed cleanup. Inspect the recorded owner and use explicit `stop --all --json` to reconcile known resources; unresolved resources remain conflicts. Do not delete execution lock or owner files as an automatic retry strategy. Status, graph, logs and retained run inspection remain available.

Agents should use the normal installed command:

```bash
go install github.com/benjaco/devflow/cmd/devflow@latest
devflow docs setup
```

Updates are intentionally Go-first:

```bash
devflow upgrade
```

Successful upgrades clear the global task artifact cache; `upgrade --json` reports `cacheCleared`. Installation failure preserves cached artifacts, and a cleanup failure is reported even if installation succeeded. APIs and worktree state follow the installed version without migrations for older formats.

Because project graph definitions are Go code, Go is expected to be available on machines where agents use Devflow.

`devflow docs setup` prints only the bundled setup/pipeline Markdown docs for the installed version. `devflow docs development` prints only the day-to-day CLI/TUI/operator docs. Both commands intentionally have no JSON mode. Use the scoped docs command that matches the task instead of fetching all docs or browsing the repository.

The intended sequencing is:
1. CLI
2. stable JSON contracts
3. typed event stream
4. TUI
5. MCP wrapper


For any failed finite JSON command, inspect `error.code`, `error.phase` and `error.message`. This includes invalid arguments, a missing/broken adapter and unknown projects/targets before execution begins. Stdout contains one result, with any available task, validation, lifecycle or repository evidence preserved. For example, `unknown_target` in `resolution` means select a declared target; `adapter_compile_failed` in `bootstrap` means fix the adapter; `resource_conflict` in `admission` means inspect the current owner. Codes come from typed failures, not message matching. The full current code table is in the contributor CLI contract.

`logs --json` and attached `watch --json` are JSONL streams, including terminal errors. Finite CI/validation still stream progress to stderr, so tools that combine streams must keep that distinction. Ctrl+C cancels direct work and its subprocesses; `operation_cancelled` and `deadline_exceeded` identify the interrupted phase. Canceling a log/watch observer or flush wait does not stop development services. Use `runs cancel` when the intention is to cancel a specific execution.

## Retained Results and Unattended Control

Engine runs, watches and actions have a `runId`; every task attempt has an `attemptId`, including finite tasks and cache decisions. Keep the IDs returned by execution or detached acceptance. An action's outer result and nested run share one run ID; a subsequent development relaunch has its own ID. An idempotent detached start returns the existing execution's ID.

```bash
devflow runs list --json
devflow runs show <run-id> --json
devflow logs <task> --run <run-id> --attempt <attempt-id> --tail 100 --json
devflow runs cancel <run-id> --json
```

`runs show` contains the selected target/mode, state, timestamps, deadline when set, graph/adapter digests, all attempts, final `result` and prompt metadata. An attempt's `executed` flag distinguishes callback execution from cache reuse; state and cache key explain its outcome. Blocked or skipped tasks without an attempt did not run. These are observations from that execution, not evidence that later edits have passed. Result/log retrieval and prompt/cancellation commands do not compile the adapter, so a newly broken adapter does not hide prior evidence.

Retained attempts use separate append-only log files; new runs and retries preserve earlier output. Retention targets 100 completed runs, seven days and 64 MiB, oldest first. Completed-run pruning runs before the current terminal result is committed, so retention failures are included in that immutable result (`retention_failed`). The newly completed result can put evidence over the count/age/byte thresholds until the next pruning pass.

Pruning first renames a completed run into a hidden retirement directory; a blocked rename preserves its published record, and failed physical deletion leaves hidden data retried by later pruning. Retired IDs return `run_expired`; following a retired attempt stops with that error. Active/interrupted records and pending physical cleanup can exceed the retention budget. Export important results/logs before pruning removes them.

Cancellation acceptance means the request was recorded; inspect `runs show` for the owner's terminal result and cleanup outcome. `runs list/show` include `ownerAlive` for nonterminal records with a recorded owner PID. If it is `false`, the interrupted record can remain nonterminal: acceptance does not prove cleanup or promise that a final result will arrive. Cancellation targets only the supplied run ID, and `run_not_active` means it already finished. Canceling an old or queued check never selects the currently active watcher as a fallback. Attached daemon runs/actions appear as queued records before admission; detached launches receive a new ID only upon admission or return the existing ID when reused. Ownership conflicts and incomplete cleanup continue to apply.

`run`, `watch` and `action run` accept `--timeout <duration>`, which reaches execution and queued admission rather than only limiting socket waits. Zero disables the operation deadline. A detached execution survives an observing CLI disconnect; use its run ID for explicit cancellation.

Prompt handling defaults to `--headless fail`: an unexpected typed prompt ends with `interaction_required` and cleanup. Supply declared action inputs first. When intentional interaction is required, select `--headless wait` and an operation timeout. Each prompt also has a five-minute wait limit. The waiting run can be inspected and answered from another CLI:

```bash
devflow prompts list --run <run-id> --json
devflow prompts respond <prompt-id> --run <run-id> --task <task> --attempt <attempt-id> --confirm true --json
```

Status includes `runId` and `pendingPrompts`. Responses must match the complete run/task/attempt/prompt identity and provide the declared answer type: `--confirm true|false`, `--text <value>`, or `--stdin` for text input without putting the answer in process arguments. Stale, duplicate, expired, canceled and mismatched answers fail. A diagnostic prompt from a failed run cannot be answered. Secret answers are transient and do not appear in ordinary prompt metadata or result/event output; adapters must avoid echoing them. Devflow never automatically confirms a question.

## Readiness Workflow

For AI coding agents, `devflow flush --json` is the readiness gate when a daemon-owned watch loop is available or desired.

Recommended loop:
1. Edit files.
2. Run `devflow flush [target] --json`.
3. If `success=true`, run focused tests or other validation commands.
4. If `success=false`, inspect `issues`, `nodes`, `services`, and referenced logs before editing again.

Require `success=true` before relying on detached watch results for downstream tests. Observation starts before the initial DAG, and flush reconciles queued and newly observed changes after startup, rebuilds, and health probes. Its result belongs to the captured daemon watch and selected project/target; a replacement watch, even with the same target, cannot satisfy the request.

`synced` and `success` are distinct: JSON `synced=true` confirms observation processing and acknowledgement, while `success=true` also requires freshness and healthy services. `watch_restart_required` means a restart policy or blocked warmup prevented changed work from rerunning. It persists across flushes until that task executes successfully or the target is explicitly restarted. `watch_stopped` means the captured watch ended or was replaced; inspect the current execution and issue a new flush. Flush issues retain `canceled` and `timeout` kinds; the command error codes are `operation_cancelled` and `deadline_exceeded`.

This guarantee is limited to declared inputs visible to polling through the final scan. It cannot prove transient or metadata-preserving edits were observed, and it does not execute tests absent from the target. Generated task outputs are excluded only when their current metadata still matches the producer's completion record. Adjacent source edits and later edits to input/output paths remain observable, including changes made while downstream tasks are still running.

For explicit service lifecycle changes, preview and then execute against the same per-worktree daemon:

```bash
devflow restart backend_debug --preview --json
devflow restart backend_debug --json
devflow stop --task backend_debug --preview --json
devflow stop --task backend_debug --json
```

The preview/result contract is `api.LifecyclePlan` plus `api.LifecycleResult`. A plan names the selected task/target and exact invalidate, stop, execute, preserve, and restart sets. The actual result reports exact affected/confirmed-stopped/restarted tasks and old/new PID plus generation; optional lifecycle `issues` explain a partial plan/result difference. Restart success means the replacement passed readiness. A restart from an already-stopped service has an empty stopped set and no previous identity. A known already-stopped task returns an empty successful stop result. An unknown task or a restart that did not create a replacement is non-success. `stop --task` leaves the daemon and independent services running; only `stop --all` performs complete cleanup. Detached launch JSON always contains `runId`, `accepted`, `daemonStarted`, `daemonPid`, `ready`, and `state`; acceptance is not readiness, so continue to use `flush` as the agent readiness gate. Status exposes the daemon through `daemon`, and `logs daemon --json` retrieves its log.

Embedded engine users that provide their own long-lived execution owner can use the same ownership primitive directly:

```go
controller := engine.NewLifecycleController()
go func() {
    errCh <- eng.Watch(ctx, engine.Request{
        Target: target, Worktree: worktree, Mode: api.ModeWatch,
        LifecycleController: controller,
    })
}()

change, err := controller.Restart(ctx, "backend_debug")
// change.Previous and change.Current contain PID + Generation;
// change.Ready is true only after the replacement readiness probe.
```

Use one controller for one active `Run` or `Watch` call and let that engine own all handles. Do not call `ServiceHandle.Stop` or kill a persisted PID in parallel with the controller. `Stop` and `Restart` return a clear error after the active engine finishes.

For prerequisite checks, prefer the same target scope the agent will use for execution:

```bash
devflow doctor --target up --json
devflow doctor --target up --strict --json
devflow clis status --target up --json
```

Target-scoped checks include `RequiredCLIs` and `RequiredEnv` attached to the selected target and its task closure, plus project-wide required env, so agents are not blocked by prerequisites needed only for unrelated targets. Use `--strict` when a missing prerequisite should fail the agent step; Devflow still emits the complete JSON report before exiting nonzero.

Avoid using attached `devflow run <service-target>` as an agent readiness gate. Attached service runs keep the terminal occupied until interrupted or until a service exits. For background development, use `devflow watch <target> --detach --json` and then `devflow flush <target> --json`; both talk to the worktree daemon. `devflow run <target> --ci --json` is finite and does not use the daemon; service tasks are started through readiness and then stopped before the command returns.

For finite test/check targets that depend on services, use `devflow run <target> --ci --json` rather than plain `run`. Plain attached `run` is for keeping services alive in an operator terminal.

In `--ci --json` mode, progress and task log lines stream to stderr and stdout remains exactly one final `RunResult`. On failure, inspect `error`, `failedNode`, `failedNodeLogPath`, and `failureExcerpts` before the terminal `logTail`. Excerpts find early test assertions, panic/fatal output, compiler errors, `AssertionError`, conventional errors/summaries, and process-failure markers even when hundreds of cleanup lines follow. An unclassified early service exit instead gets a `process-exit-tail` excerpt with up to 12 meaningful terminal lines. All excerpts remain within five windows, 200 lines, 64 KiB total, and 8 KiB per line and redact known environment secrets and PostgreSQL URLs; empty logs produce `[]`. The `nodes` array supplies every selected node's final state and duration; downstream work skipped after a dependency failure is `blocked` with the dependency named in `lastError`, while unrelated interrupted work is `canceled`. Cacheable nodes include hit/miss and key/read/write/manifest timing. Use `devflow logs` only when both bounded diagnostics are insufficient.

`devflow logs <task> --tail 100 --follow --json` emits JSONL records containing `task`, `line` and available run/attempt identity, preserving blank lines and continuing after the initial tail without replay. Omit `--follow` for a finite read; `--tail 0` streams all currently available lines. Use `--run` and optionally `--attempt` to retrieve a retained attempt instead of the current one. Selection happens once: `--follow` stays on that attempt after a restart; reissue the command or select the newer attempt to see its output. A finite read includes the last partial line, while follow waits for its newline so characters split across writes stay intact. Cancellation stops this reader without stopping development services. Individual lines above 4 MiB fail explicitly instead of silently losing text. Following detects observed truncation/replacement, but polling cannot recover external rewrites; normal task attempts now have separate retained logs. Following across attempts and resumable log cursors remain planned for item 7.

For CI jobs that intentionally repair generated or formatted repository files, use the atomic repository mode instead of scripting status/add/commit/push around Devflow:

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

The repository must be clean before the DAG starts. Devflow never commits or pushes a failed DAG, stages only paths matched by the repeated Git pathspecs, and rejects tracked changes elsewhere. CRLF/LF-only tracked changes are ignored by default across both permitted and unexpected paths, removed from the index if a task staged them, and left modified but unstaged in the worktree; pass `--pedantic` to make them commit-worthy. A successful DAG with no material permitted changes remains success, does not push, and does not trigger `--fail-after-commit`. When CI has no Git identity, the new commit inherits author/committer attribution from `HEAD`.

Inspect `repositoryChanges.status` and its booleans before deciding what happened. In particular, `push_failed` with `commitCreated=true`, a non-empty `commitSha`, `pushAttempted=true`, and `pushSucceeded=false` means the repair commit exists locally but was not pushed. `failed_after_commit` is the requested deliberate nonzero state after the commit and any requested push succeeded. `changedPathCount`, `ignoredLineEndingPathCount`, and `unexpectedTrackedPathCount` are exact; their corresponding arrays are bounded and carry truncation flags. `pedantic` records whether line endings were treated byte-sensitively. As with every direct CI JSON run, the final JSON is alone on stdout and all DAG/Git progress is on stderr.

When an agent changes `devflow.project.go` inputs, outputs, or dependency edges for a finite target, use the dedicated hardening surface:

```bash
devflow validate build --mode all --details issues --max-listed-paths 200 --json
```

JSON defaults to `--details issues`: use exact count fields for totals, `samples` for actionable paths, and `truncated` to tell which exhaustive arrays were omitted. Use `--details summary` when only state/count/timing/resource data is needed, or `--details full` only when an exhaustive path list is intentional. Treat success as meaningful only when `orders.complete=true`. If the valid-order count exceeds the default bound, Devflow runs no permutations and returns an `order_limit_exceeded` issue; raise `--max-orders` explicitly if repeated execution is safe. Artifact results identify projected inputs, dependency outputs, undeclared writes, and missing outputs. Order results identify the exact permutation and failed task or the final artifact paths that differed.

Validation bypasses caches/stamps and runs tasks repeatedly in disposable worktree sandboxes. Phase/counter progress goes to stderr while the single final JSON stays on stdout. Check `metrics` for cumulative files/logical bytes, current/peak temporary storage, measured physical allocation where `temporaryPhysicalBytesMeasured=true`, remaining limits, and phase timing. A `resourceFailure` identifies the phase and exceeded budget/disk reserve. It rejects service/debug-service targets and does not isolate databases, networks, global caches, absolute paths, or unregistered background processes, so agents must choose finite targets with safe external effects.

Validation includes each task's `BeforeRun` and optional `Run`, including hook-only tasks. Hook-provided runtime environment values stay local to the task, and a failing hook prevents `Run`. Artifact findings include final hook writes; task/order errors and captured logs include hook diagnostics. Treat those failures as evidence about the complete callback sequence. Prompts fail immediately, and service readiness and `AfterReady` are outside this finite validation path. A finite task that registers supervised handles fails validation; inspect its error for cleanup failures as well as the callback failure before assuming those resources stopped.

`AGENTS.md` documents repository rules for coding agents. Future milestones can add project skills under `agents/skills/`.

For agents contributing to this repository, `docs_contributors/agent-memory.md` is shared long-term project memory. Read it before substantial work and update it when durable project context, mental models, or recurring constraints change.

For CI cache integration, use the supported introspection commands instead of reconstructing keys or platform cache paths. When semantic fingerprints are expensive, always pair the manifest-producing preflight with the immediately following run:

```bash
devflow cache path --json
devflow cache key --target build --manifest-out "$RUNNER_TEMP/devflow-cache-manifest.json" --json
# Restore the external cache namespace using the returned key, then:
devflow run build --cache-key-manifest "$RUNNER_TEMP/devflow-cache-manifest.json" --ci --json
```

The manifest is an owner-only, 15-minute, same-build/project/worktree/target snapshot. It stores environment and semantic values only as hashes. An explicit wrong, changed, corrupt, or expired manifest fails instead of rerunning remote callbacks. The run still recomputes local inputs and dependency keys, so generated-input DAGs remain correct. In the final result, `cacheKeyManifest.reusedComponents` proves reuse and `cacheKeyManifest.localInputChangedTasks` identifies tasks whose cheap local inputs changed after preflight; each node's `cache.manifestComponents` names reused components.

For embedded Go callers, the complete handoff is:

```go
keyResult, manifest, err := eng.CacheKeyWithManifest(ctx, engine.Request{
    Target: target, Worktree: worktree, Mode: api.ModeCI,
})
if err != nil { /* handle */ }

err = engine.WriteCacheKeyManifest(manifestPath, manifest) // atomic and 0600
if err != nil { /* handle */ }

outcome, err := eng.Run(ctx, engine.Request{
    Target: target, Worktree: worktree, Mode: api.ModeCI,
    CacheKeyManifestPath: manifestPath,
})
```

Use `keyResult.Key` for external cache restore. Inspect `outcome.Result.CacheKeyManifest` for reuse. Do not persist or edit the manifest, reuse it across jobs, or trust its preflight final task keys as durable keys; Devflow validates and rebuilds the execution keys.

Embedded validation callers use the same `issues` default as JSON commands. Set `Details: api.ValidationDetailsFull` only when exhaustive paths are needed; bounded details can also be explicit:

```go
result, err := validator.Run(ctx, validation.Request{
    Target: target, Worktree: worktree,
    Mode: api.ValidationModeAll,
    Details: api.ValidationDetailsIssues,
    MaxListedPaths: validation.DefaultMaxListedPaths,
    OnEvent: func(event api.Event) { /* validation_progress */ },
})
```
