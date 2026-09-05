# Startup and flush freshness verification

Item 2 of the [approved sequence](agent-verification-plan.md), implemented on 2026-09-05–06. Item 1 and its Windows CI correction were accepted before this work. Item 3 has not started.

## Observed failures and regressions

These failures were observed while developing the fix. The tests use temporary worktrees, explicit execution gates and isolated cache homes; they do not operate on existing development services.

| Scenario | Observed failure before correction | Regression |
| --- | --- | --- |
| Edit after startup task reads its input | Flush succeeded with the old artifact and only one execution | `TestWatchFlushIncludesChangesDuringInitialRun/initial_success` |
| Repair input while the first attempt is failing | The repaired artifact was never built | `TestWatchFlushIncludesChangesDuringInitialRun/initial_failure` |
| Second edit during a blocked rebuild | Flush accepted the first edit's artifact; two runs instead of three | `TestWatchFlushIncludesChangesDuringRebuild` |
| Restart policy prevents refresh | First and repeated flushes succeeded despite unresolved input changes | `TestWatchFlushReportsChangesBlockedByRestartPolicy` |
| Generated output and neighboring source share a directory | Explicit file outputs hid the later sibling edit; path outputs caused duplicate service restarts | `TestWatchGeneratedOutputDoesNotHideSiblingSourceEdits` |
| Edit an input/output file after formatting finishes, while its consumer is paused | Flush succeeded with `checked.txt="FIRST SOURCE EDIT"` despite newer source; two runs instead of three | `TestWatchFlushPreservesInPlaceInputEditAfterFormatterCompletes` |
| Edit an input/output file after `Run` returns, while its cache snapshot is paused | Flush succeeded with `checked.txt="FIRST EDIT"` despite newer source; two runs instead of three | `TestWatchFlushPreservesInPlaceEditDuringCacheSnapshot` |
| Restart stop error includes cancellation; final cleanup succeeds | Watch returned a nil error | `TestWatchPreservesInterruptedRestartFailure` |
| Output child changes alter an existing parent directory's mtime | Scanner reported both `src` and `src/generated`, invalidating unrelated siblings | `TestDirectoryChildChangesDoNotReportUnchangedParent` |
| Enumerated child disappears before its metadata is read | Both directory-walk and entry-info cases returned file-not-found errors | `TestScanEntryHandlesConcurrentDisappearance` |
| Flush targets a starting, replaced or different-project watch | Observer readiness, captured execution identity, cancellation or project checks were missing | `TestFlushWaitsForCapturedWatchObserver`, `TestFlushRejectsReplacementWatchAcknowledgement`, `TestFlushCancellationWhileWaitingForAck`, `TestFlushRejectsDifferentProject` |

Additional acceptance tests cover concurrent flushes sharing the latest artifact/cache key, an input edit during a paused readiness probe, stopped watches and request publication during replacement, full watcher queues, combined queued/pending/unpolled changes, canceled synchronization and retained changes after scan errors. Output evidence tests cover post-completion edits/deletions, newly created versus preexisting ancestors, incomplete scans and symlinks. Scanner failures must still clean up registered resources.

## Focused verification

```sh
go test ./pkg/engine -run 'TestWatch(FlushIncludesChanges|ConcurrentFlushes|FlushRechecksInputs|FlushPreservesInPlace|FlushReportsChangesBlocked|GeneratedOutput|Output|PreservesInterrupted)' -count=1 -v
go test ./pkg/engine -run TestExecutionOwnershipWatchScanFailureCleansResources -count=1 -v
go test ./pkg/watch -run 'Test(RunnerSync|Directory|ScanEntry|RunnerRejects|RunnerAllows)' -count=1 -v
go test ./pkg/daemon -run 'Test(Flush|WaitForFlushAck)' -count=1 -v
```

Passed on Go 1.27.1 darwin/arm64: `go test -count=1 ./...`, `go test -race -count=1 ./...`, `go vet ./...`, Staticcheck v0.8.1, govulncheck v1.6.0 (no vulnerabilities), clean module/format/diff checks and the version JSON smoke. Both full suites include the examples. Linux/Windows amd64 watcher, engine and daemon test binaries and the CLI compile successfully. Staticcheck initially found the unused generated-output fixture; it was removed and the check passed. Native Linux/Windows execution remains a CI check; compilation is not execution. `PROGRESS.md` records completion and the review handoff.

## Resulting boundary

The observer baseline precedes the initial DAG. Reconciliation combines queued and pending events with a fresh scan after execution and after health probes before acknowledging flush. Daemon flush waits for and remains bound to its captured watch. The timestamp-readiness and sentinel-retouch workaround is removed.

`synced=true` means the request's observation boundary was processed. Require `success=true` for freshness and health. `watch_restart_required` persists when restart/warmup policy prevents required work; a repeated flush cannot make those old results fresh. Stop/restart the target or explicitly rerun the affected work.

Declared output events are suppressed using each producer's completion metadata, captured before finite-task cache persistence and preserving subsequent edits while downstream work continues. There is no timed suppression window or blanket parent-subtree exclusion. Output declarations establish producer ownership during its own execution; metadata evidence cannot attribute external writes made inside that interval. Polling covers declared inputs and observed metadata changes; it cannot prove transient or metadata-preserving edits, undeclared dependencies, external state or changes after the final scan. A later edit requires another flush. This change adds no run history, planner or validation lifecycle work.
