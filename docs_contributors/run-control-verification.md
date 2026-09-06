# Run evidence and unattended control verification

Item 5 of the approved agent-verification plan. Baseline: `3965a65`, accepted item 4. Item 6 is intentionally separate.

## Observed failures before fixes

| Regression | Observed failure | Required behavior |
| --- | --- | --- |
| Earlier attempt logs | `TestRunsPreserveEarlierAttemptLogs` reused the same path; the first failure text was replaced by the second execution's output | Distinct attempt paths; earlier bytes remain available |
| Parallel prompts | `TestParallelTaskPromptsHaveDistinctIdentity` received `prompt-1` from two different tasks | Run/task/attempt-bound unique prompt identities |
| Interactive rejection | `TestInteractivePromptFailureStopsOwnedProcess` reached the two-second context deadline after its prompt handler rejected input | Stop the owned process and return the prompt failure promptly |
| Secret response echo | Removing suppression let `Hello, private-answer` reach both task log and event | Suppress subprocess output before submitting a secret answer |
| Operation timeout | `TestRunRequestDeadlineCancelsExecution` showed attached/detached execution ignoring `TimeoutMs` | Deadline reaches the task and retained cancellation evidence |
| Early flush deadline | The full suite returned `timedOut=false` when the existing deadline expired during watcher admission; deterministic pre-admission cancellation/deadline cases reproduced it | Classify cancellation and timeout consistently at every flush phase |
| Detached identity | `TestDetachedRunReturnsStableExecutionIdentity` returned no run ID | Acceptance and later results identify the same execution |
| Stopped/replaced prompt attempt | Old prompts remained pending, accepted responses and delivered `stale-secret` after their attempt stopped or was superseded | Reject creation/response/consumption and close stale pending metadata |
| Partial retention deletion | A simulated locked-log deletion removed `record.json`, poisoning run listing; blocked retirement was not respected | Atomically retire before recursive deletion; retain complete original on rename failure |
| Terminal retention error | Maintenance failure was returned after immutable success had already been saved | Prune earlier and include the maintenance error in the terminal result |
| Followed log expiry | A followed retired attempt waited until the observer deadline | Return typed `run_expired` |
| Reused readiness | `TestDetachedReadinessCannotReusePreviousRun` returned `ready=true` using previous same-target status | Readiness belongs to the accepted run ID |

The log test was run against an isolated archive of the baseline with only that regression test added. The original parallel-ID and deadline tests were run before implementation. Secret suppression, stale-attempt rejection and final-event ordering were tested against the incomplete new protocol before their corresponding fixes. Store and new public-command tests were added first and failed against deliberately incomplete new handlers before implementation; these demonstrate test-first development, not pre-existing command defects.

## Additional regression coverage

- Atomic record publication/readers, immutable terminal results, unique claimed identities, malformed/unknown/expired IDs, and count/age/byte pruning of completed evidence only. A retained counter classifies expired IDs without per-run tombstones. Active and interrupted records remain protected.
- Retained executed/cache-hit attempts contain the already-computed key, timestamps, graph digest and executable digest. Later runs cannot rewrite earlier results. A queued identity cannot be claimed for a different target/project/mode.
- Scoped direct/daemon cancellation while running, queued or waiting; queued cancellation/deadlines return before the transition lock is released and preserve the current watcher. Repeated cancellation requests remain scoped. Detached observer disconnect does not terminate execution.
- Real subprocess headless rejection, default `interaction_required`, bounded explicit wait, concurrent prompts, persisted reconnect, typed false/empty answers, stale/duplicate/mismatched/expired rejection, and cancellation cleanup of transient answer files.
- Public CLI list/show/cancel and prompt list/respond work without loading a broken adapter. Historical logs carry run/attempt IDs. Direct CLI flags reach execution and retained terminal evidence; cancellation during a paused Git finalization child preserves HEAD and creates no repair commit/push; `--stdin` supports secret input without argv or ordinary response output.
- TUI recovery of pending prompts, parallel prompt queueing, explicit wait requests and masked secret input. Existing example/validation callers use the current prompt API.

## Verification commands

```bash
go test ./pkg/engine -run 'TestRunsPreserve|TestRetainedRuns|TestRunIdentity|TestDirectRunCancellation|TestParallelTaskPrompts|TestHeadless|TestWaitingPrompt' -count=1
go test ./pkg/daemon -run 'TestRunRequestDeadline|TestQueued|TestActionAndExecution|TestWaitingAction|TestDetached' -count=1
go test ./internal/cli -run 'TestRuns|TestPrompt|TestLogs|TestDirectCLI' -count=1
go test ./pkg/instance ./pkg/engine ./internal/cli -run 'TestRunPrune|TestRetentionFailure|TestFollowedRetained' -count=1
go test ./pkg/daemon -run 'TestFlushClassifiesCancellationBeforeWatchAdmission' -count=1
go test -race ./pkg/instance ./pkg/process ./pkg/daemon -count=1
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
go mod tidy -diff
go run ./cmd/devflow version --json
git diff --check
```

Focused regressions, final full default/race suites (including examples), vet, Staticcheck v0.8.1, govulncheck v1.6.0 (no vulnerabilities), module/format/diff checks and version JSON pass. Linux/Windows amd64 CLI and affected CLI/engine/daemon/instance/process/TUI/validation/PayloadCMS test binaries compile. These are local results; native Windows execution remains a CI check. Freeze Go source edits before full CLI verification because adapter bootstrap keys include repository source.

Full verification includes the final atomic-retention, followed-log expiry and early flush-deadline fixes. The unchanged CLI flush timeout regression also passed ten consecutive focused runs. The first full run exposed one obsolete fixture assumption: a fake daemon log relied on task execution creating the daemon diagnostic directory. The fixture now creates its own directory; its focused and full reruns pass.

## Boundaries

Retention targets 100 completed runs, seven days and 64 MiB of completed evidence; it runs before terminal publication. Active/interrupted runs and their logs can exceed those bounds. A dead owner PID does not prove child or external-resource cleanup, so listing does not finalize or prune that work. IDs identify past evidence, not freshness after later edits. Completed-run pruning runs before the current terminal result is committed, so retention failures are included in that immutable result (`retention_failed`). The newly completed result can put evidence over the count/age/byte thresholds until the next pruning pass.

Pruning first renames a completed run into a hidden retirement directory; a blocked rename preserves its published record, and failed physical deletion leaves hidden data retried by later pruning. Retired IDs return `run_expired`; following a retired attempt stops with that error. Active/interrupted records and pending physical cleanup can exceed the retention budget.

`logs --follow` pins the selected attempt. Item 7 adds finite cursors and compact result/progress controls together with the five later review corrections; see [compact evidence verification](compact-evidence-verification.md). Cross-attempt following remains separate. Same-worktree overlap remains rejected. Adapters must honor execution contexts and registered resource cleanup; a deadline cannot forcibly interrupt arbitrary Go callbacks or prove external resource shutdown. Native Windows file/process/console behavior and real Docker/PTY workflows require their corresponding CI or opt-in execution; cross-compilation alone is insufficient.
