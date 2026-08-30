# Agent Integration

Devflow is designed so humans and agents use the same execution surface:
- operational CLI commands have stable JSON output
- instance and task state are persisted
- logs are addressable by instance and task
- one per-worktree daemon owns mutable dev/watch/operator work and publishes a typed event stream for live consumers
- `run --ci` is the exception: it stays direct and finite for CI-style validation

Agents should use the normal installed command:

```bash
go install github.com/benjaco/devflow/cmd/devflow@latest
devflow docs setup
```

Updates are intentionally Go-first:

```bash
devflow upgrade
```

Because project graph definitions are Go code, Go is expected to be available on machines where agents use Devflow.

`devflow docs setup` prints only the bundled setup/pipeline Markdown docs for the installed version. `devflow docs development` prints only the day-to-day CLI/TUI/operator docs. Both commands intentionally have no JSON mode. Use the scoped docs command that matches the task instead of fetching all docs or browsing the repository.

The intended sequencing is:
1. CLI
2. stable JSON contracts
3. typed event stream
4. TUI
5. MCP wrapper

## Readiness Workflow

For AI coding agents, `devflow flush --json` is the readiness gate when a daemon-owned watch loop is available or desired.

Recommended loop:
1. Edit files.
2. Run `devflow flush [target] --json`.
3. If `success=true`, run focused tests or other validation commands.
4. If `success=false`, inspect `issues`, `nodes`, `services`, and referenced logs before editing again.

Do not run downstream tests before a successful flush when relying on detached watch/dev mode. The flush sync sentinel proves the watcher has observed the post-edit boundary and has settled the selected target closure.

For explicit service lifecycle changes, preview and then execute against the same per-worktree daemon:

```bash
devflow restart backend_debug --preview --json
devflow restart backend_debug --json
devflow stop --task backend_debug --preview --json
devflow stop --task backend_debug --json
```

The preview/result contract is `api.LifecyclePlan` plus `api.LifecycleResult`. A plan names the selected task/target and exact invalidate, stop, execute, preserve, and restart sets. The actual result reports exact affected/confirmed-stopped/restarted tasks and old/new PID plus generation; optional lifecycle `issues` explain a partial plan/result difference. Restart success means the replacement passed readiness. A restart from an already-stopped service has an empty stopped set and no previous identity. A known already-stopped task returns an empty successful stop result. An unknown task or a restart that did not create a replacement is non-success. `stop --task` leaves the daemon and independent services running; only `stop --all` performs complete cleanup. Detached launch JSON always contains `accepted`, `supervisorStarted`, `ready`, and `state`; acceptance is not readiness, so continue to use `flush` as the agent readiness gate.

Embedded engine users that intentionally provide their own long-lived supervisor can use the same ownership primitive directly:

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

The repository must be clean before the DAG starts. Devflow never commits or pushes a failed DAG, stages only paths matched by the repeated Git pathspecs, and rejects tracked changes elsewhere. A successful DAG with no permitted changes remains success, does not push, and does not trigger `--fail-after-commit`. When CI has no Git identity, the new commit inherits author/committer attribution from `HEAD`.

Inspect `repositoryChanges.status` and its booleans before deciding what happened. In particular, `push_failed` with `commitCreated=true`, a non-empty `commitSha`, `pushAttempted=true`, and `pushSucceeded=false` means the repair commit exists locally but was not pushed. `failed_after_commit` is the requested deliberate nonzero state after the commit and any requested push succeeded. `changedPathCount` and `unexpectedTrackedPathCount` are exact; their corresponding arrays are bounded and carry truncation flags. As with every direct CI JSON run, the final JSON is alone on stdout and all DAG/Git progress is on stderr.

When an agent changes `devflow.project.go` inputs, outputs, or dependency edges for a finite target, use the dedicated hardening surface:

```bash
devflow validate build --mode all --details issues --max-listed-paths 200 --json
```

JSON defaults to `--details issues`: use exact count fields for totals, `samples` for actionable paths, and `truncated` to tell which exhaustive arrays were omitted. Use `--details summary` when only state/count/timing/resource data is needed, or `--details full` only when an exhaustive path list is intentional. Treat success as meaningful only when `orders.complete=true`. If the valid-order count exceeds the default bound, Devflow runs no permutations and returns an `order_limit_exceeded` issue; raise `--max-orders` explicitly if repeated execution is safe. Artifact results identify projected inputs, dependency outputs, undeclared writes, and missing outputs. Order results identify the exact permutation and failed task or the final artifact paths that differed.

Validation bypasses caches/stamps and runs tasks repeatedly in disposable worktree sandboxes. Phase/counter progress goes to stderr while the single final JSON stays on stdout. Check `metrics` for cumulative files/logical bytes, current/peak temporary storage, measured physical allocation where `temporaryPhysicalBytesMeasured=true`, remaining limits, and phase timing. A `resourceFailure` identifies the phase and exceeded budget/disk reserve. It rejects service/debug-service targets and does not isolate databases, networks, global caches, absolute paths, or unregistered background processes, so agents must choose finite targets with safe external effects.

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

Embedded validation callers should set bounded details explicitly because the zero value preserves historical exhaustive output:

```go
result, err := validator.Run(ctx, validation.Request{
    Target: target, Worktree: worktree,
    Mode: api.ValidationModeAll,
    Details: api.ValidationDetailsIssues,
    MaxListedPaths: validation.DefaultMaxListedPaths,
    OnEvent: func(event api.Event) { /* validation_progress */ },
})
```
