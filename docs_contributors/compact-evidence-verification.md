# Compact evidence and review corrections

Item 7 and the five outstanding item 5 findings were explicitly approved as one
change after item 6's Windows correction (`e04dee3`). The changes share retained
run/attempt identity; compact presentation never replaces full stored evidence.

## Observed failures before fixes

| Regression | Observed failure |
| --- | --- |
| `TestQueuedLifecycleDeadlinePreservesCurrentWatcher` | Invalidation waited past its deadline and stopped the watcher; restart and retarget also bypassed cancellable admission. |
| `TestCurrentLogIdentityUsesOneStatusSnapshot` | Old log bytes were labeled with a newly selected attempt ID. |
| `TestLifecycleRestartPreservesCompletedAttempts` | Restart rewrote the completed predecessor to `starting`. |
| `TestPromptRejectsResponseAfterOwnerExited` | A dead owner remained answerable and accepted a secret response. |
| `TestRunCancellationRemovesUndeliveredPromptAnswers` | Cancellation retained accepted-but-unconsumed answer files, including retries. |
| `TestPromptRejectsResponseWithoutOwner` | Missing owner metadata still admitted prompt input. |
| `TestFinalEvidenceSaveFailureChangesSuccessfulResult` | Final engine SaveRun rejection returned `Success=true`, `Error=nil`. |
| `TestDaemonCompletionErrorsInvalidateReturnedResult` | Failed retained-record reads and a denied final write left observers with a successful result. |
| `TestResultDetailsHealthyStatusIsBounded`, `TestResultDetailsRunPreservesRetainedEvidence`, `TestProgressControlsTaskLogsIndependently` | CLI rejected the new details/progress flags. |
| Log page tests | Page API initially did not compile; CLI rejected `--max-bytes`. |
| Compact review probes | Unsampled target text produced a 1 MiB summary; exhausted text budget erased a prompt's task identity; early errors bypassed the view bound. |
| Issues-detail regression | The new issues view initially omitted diagnostic excerpts. |

These were run against the existing or incomplete implementation before the
corresponding fix. Temporary review probes were converted to permanent
`compact_bounds_test.go` regressions. No failure counts are inferred from source
inspection alone.

## Coverage and verification

Compact tests exercise 2,000-node healthy/failing graphs, primary failures before
blocked dependents, exact counts, explicit truncation, full defaults, retained
failure excerpts, preserved flush sync/timeout state, usable prompt identities,
and quiet progress with a visible final failure. Compiled tests run a real
adapter, retrieve its historical result from another cwd, preserve text failure
diagnostics through bootstrap and inspect daemon flush responses.

Page tests cover UTF-8 and partial lines, repeatable offsets, append-after-EOF,
current-status replacement, mismatched identities, malformed and oversized
cursors, bounded reads of sparse logs, observed rewrite/shrink/replacement,
terminal malformed bytes, cancellation and pruning. Existing line/follow tests
remain in the focused race suite. See the CLI contract for byte and text limits.

Useful focused commands:

```sh
go test ./pkg/daemon ./pkg/engine -run 'TestQueuedLifecycle|TestLifecycleRestartPreserves|TestFinalEvidence|TestDaemonCompletion' -count=1
go test ./pkg/instance -run 'TestPrompt|TestRunCancellation' -count=1
go test ./internal/cli ./internal/logstream -run 'TestResult|TestCompact|TestProgress|TestPage|TestLogPages|TestCurrentLogIdentity' -count=1
go test ./internal/cli -run 'TestBootstrapCompact|TestCompiledCompact' -count=1
```

Final full default/race suites (including examples), compiled boundary tests,
vet, Staticcheck v0.8.1, govulncheck v1.6.0 (no vulnerabilities), formatting,
module/diff/version checks and 14 Linux/Windows affected test/CLI compilation
checks passed. Results are also recorded in `PROGRESS.md`. Native Windows filesystem, process and console semantics require
CI; cross-compilation only proves buildability. Tests use isolated temporary
worktrees and process fixtures, not existing development services.

## Limits

Cursors remain within one immutable attempt. Their bounded observations cannot
detect arbitrary external rewrites that preserve checked bytes; they are not a
filesystem audit log. An existing-service lifecycle command, once admitted,
executes in its owning engine context; the queued-admission fixes do not introduce
an independent deadline for that service command. CLI restart exposes no such
timeout option. Broader cross-attempt streaming and service-command execution
deadlines remain separate work.
