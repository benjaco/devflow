# Automatic GitHub CI presentation verification

Local implementation baseline: `431603d`. The user-selected solution is a
presentation change to one Devflow invocation, preserving the scheduler and
execution owner. It replaces the proposed composite-action export work.

## Observed failures before fixes

| Regression | Observed failure |
| --- | --- |
| `TestGitHubCIAutomaticSelectionAndProgressControls` | `GITHUB_ACTIONS=true` still printed flat task lines; expected completed-attempt groups were absent. |
| `TestAttemptCompletion*` | No explicit attempt-log completion events existed for finite, cached, service or canceled work. |
| `TestAttemptCompletionDoesNotCloseLogBeforeTimedOutReadinessReturns` | An initial completion implementation marked a failed attempt complete while its timed-out readiness callback was still held at a barrier and could append logs. |
| `TestGitHubPresenterPreservesUnownedProgress` | Task-labeled setup output without an attempt identity disappeared; repository-repair lines gained a duplicate `[devflow]` prefix. |
| Final-output helper regressions | Legacy runner commands remained literal inside final JSON values; human compact output repeated task excerpts after their groups. |
| Compiled demo bootstrap | A library package in the demo adapter conflicted with the generated `main` package. The demo now uses the actual local-adapter package contract, with a separate ordinary entrypoint for repository builds. |
| `TestGitHubCICompletedHeadersReplaceMissAndDoneLines` (baseline `ee561d9`) | Grouped CI printed separate cache-miss and `done` lines, while completion headers omitted the cache outcome. |
| `TestBeginAttemptSaveFailureDoesNotInheritCacheOutcome` | A new attempt whose initial evidence save failed inherited the predecessor's cache miss before performing any lookup. Clear cache metadata when allocating the new identity. |

All were exercised before their respective fixes. New formatter and queue tests
also began with missing/no-op helpers before implementation; they are separate
from the reproduced existing-behavior failures above.

The first concurrent full normal/race gates also exposed an I/O-heavy test
fixture: the slow-replay regression reopened its retained log 2,048 times and
exceeded its 15-second deadline under load. It now uses one open retained file
and `Runtime.EventLineEmitter`, matching subprocess logging while preserving
all 2,048 events, the barrier, and the original deadline. Ten concurrent repeated
normal and race checks passed after that fixture correction.

## PR #15 fixture cleanup

[Run `34213141067`, pinned Linux job `102018585350`](https://github.com/benjaco/devflow/actions/runs/34213141067/job/102018585350)
at `81b6ff5` failed only `TestGitHubEnvironmentDoesNotSelectCIMode`: temporary-directory
cleanup reported `.devflow: directory not empty`; its behavior assertions passed.
The other seven jobs passed. The stop response preceded the daemon's final log write.

Before the fix, 900 unchanged local repetitions with default scheduling,
`GOMAXPROCS=1` and `GOMAXPROCS=8` passed. A controlled real-protocol reproduction held the response ACK,
removed `.devflow`, then released the ACK: the shutdown log recreated the directory.
This reproduced the late-write cause on macOS, rather than the intermittent Linux
`unlinkat` error itself. Releasing the ACK and waiting for disconnection before removal
passed 20 controlled repetitions. The fixture now reuses `stopJSONContractDaemon`;
no runtime or workflow behavior changed. Local Docker was unavailable, so native
Linux execution remains a hosted check.

Focused verification of the corrected fixture:

```sh
go test ./internal/cli -run '^TestGitHubEnvironmentDoesNotSelectCIMode$' -count=100
```

## Windows cache-name correction

[Run 34195733262, Windows job 101962921185](https://github.com/benjaco/devflow/actions/runs/34195733262/job/101962921185)
at `9ce809a` failed `TestBootstrapGitHubCIPresentationUsesInvocationEnvironment`:
the cached `shared:generate` task was rejected with
`cache task "shared:generate" must be a single local path component`.
All seven other jobs passed. The failure also occurred with GitHub presentation
disabled; cache storage had treated a logical task name as a native filename.

The new `pkg/cache/task_identity_test.go` tests failed against that implementation
for colon names, case collisions, Windows device names, trailing dots/spaces and
oversized names. The cache now maps every task name to a fixed-length SHA-256
directory while retaining its exact logical name in manifests and JSON. Listing
and GC verify that identity before using it, and invalidation uses the same
mapping. Cache-key and output-path validation remain separate. There is no old
layout fallback or migration; older disposable artifacts are rebuilt.

Portable regressions exercise snapshot/load/restore/list/GC/invalidate across
16 names, deterministic GC ordering, scoped invalidation and forged manifest
identity rejection. The pre-existing colon-output test remains host-specific;
colon task-name coverage now runs on Windows too. The compiled demo test keeps
`shared:generate` unchanged. Final local gates are recorded in `PROGRESS.md`;
cross-compilation does not replace the required native Windows CI rerun.

## Permanent coverage

- Invocation true/false/empty/unset selection, text/JSON, quiet/states/logs,
  project-local executable bootstrap, adapter runtime env independence, unchanged
  non-CI mode and raw log/watch JSONL contracts.
- Barrier-based overlap of two tasks with one shared dependency, independently
  recorded attempt intervals, and a blocked replay writer while a sibling emits
  2,048 lines and finishes. No wall-clock speed threshold establishes concurrency.
- A fixed 128-entry presentation queue overwhelmed by 384 distinct attempts of
  the same task, duplicate completion notifications and 10,000 live log events:
  final reconciliation produces one group per attempt without copied live logs.
- Cache miss/hit, stamp reuse with its recorded outcome, skipped-state formatting,
  aliases without fabricated attempts,
  failures and blocked dependents, cancellation with available logs, service
  readiness followed by stop, delayed writer drainage, and timed-out readiness
  callbacks whose completion remains explicitly unknown.
- Whole group boundaries, 12 MiB streamed logs, the existing 4 MiB line limit,
  blank/partial/CR lines, child/legacy markers, command-title/message escaping,
  deliberate failure annotations and unchanged retained bytes.
- Final JSON preserves decoded values while legacy marker bytes stay inert;
  human summaries avoid a second excerpt replay. Read/write failures remain
  presentation diagnostics, and a repository `--fail-after-commit` result stays
  failed even when every task succeeded.
- A 10,005-node summary keeps exact counts with at most 200 rows, bounded escaped
  cells and explicit omissions. Summary appends preserve existing content and
  diagnose the GitHub 1 MiB step limit without overwriting data.

Focused commands:

```sh
go test ./internal/cli -run 'TestGitHub|TestBootstrapGitHub|TestCompiledGitHub' -count=1
go test -race ./internal/cli -run 'TestGitHub|TestBootstrapGitHub|TestCompiledGitHub' -count=1
go test -race ./pkg/engine -run 'TestAttemptCompletion|TestLifecycleRestartPreservesCompletedAttempts' -count=1
```

Freeze Go source and bundled user docs before bootstrap/full checks. Final
repository gates and cross-platform compilation results are recorded in
`PROGRESS.md`; native Windows execution and actual hosted rendering are distinct
from local compilation/output tests.

## Demo and hosted check

The [portable demo](../examples/github-actions/README.md) uses local Go callbacks
and a cached shared generator. Its ordinary command is:

```sh
devflow run verify --ci --json
```

`examples/github-actions/workflow-smoke.yml.example` is an inactive example
prepared for a manual hosted check. It binds a source-built binary to this
checkout for adapter compilation and uses the same single Devflow command.
No exporter, coordinator, generated action, Checks API, runner action markers or
TUI changes are part of this feature. No remote run is authorized by this local task.

An actual hosted run must confirm rendered collapsible groups, textual
status/duration titles, the final job-summary table and normal step exit status.
Local verification checks the emitted protocol and execution evidence only.

The isolated local demo exercised the ordinary command above twice (cache miss,
then hit), followed by `devflow run failure --ci --json`. Exit codes were 0, 0,
and 1; each attempt had one closed group, both check outputs appeared once, the
failure emitted one annotation, all stdout documents decoded, and three final
tables appended to the summary. Generated demo state stayed outside the checkout.

## Boundaries and references

Slow output may defer live updates and delays final CLI draining, but it does not
backpressure task execution through the presenter. The collector keeps references
and bounded text; full logs are streamed from retained files. Incomplete output
is a finite available snapshot. Arbitrary unregistered adapter goroutines are
outside the writer-completion contract. No presentation error replaces the real
execution result.

Protocol references: [GitHub workflow commands](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-commands),
[GitHub variables](https://docs.github.com/en/actions/reference/workflows-and-actions/variables),
[toolkit escaping](https://github.com/actions/toolkit/blob/main/packages/core/src/command.ts)
and [runner command parsing](https://github.com/actions/runner/blob/main/src/Runner.Common/ActionCommand.cs).
Ordinary groups provide textual titles; runner-owned nested action markers from
[runner PR 4243](https://github.com/actions/runner/pull/4243) are outside scope.
