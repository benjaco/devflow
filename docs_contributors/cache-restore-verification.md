# Cache restore verification and scope

The portable test at `cdc6ad5` failed in [native Windows CI](https://github.com/benjaco/devflow/actions/runs/34642649058/job/103405841095): an ordinary open reader prevented the output-directory backup rename, the attempt remained unexecuted, and the retained log contained only copy counters. Closing the reader allowed the same cache to restore successfully. Linux and macOS ran the same scenario successfully. The fixture establishes its own reader as the cause; the original application's lock holder remains unknown.

## What testing changed

The original proposed patch was broader than required. Existing restoration already preserved originals/cache on the reported failure, rolled back earlier replacements and retained backups when recovery failed. Completed failed attempts also already survived later watcher cancellation.

The implemented change keeps:

- bounded, cancellation-aware retries of the rename itself using the existing Windows two-second policy; permission preparation and destination removal happen once
- ordinary wrapped backup/install/rollback diagnostics, plus bounded, redacted task-log evidence when restoration fails before callback execution
- a recovery context independent of forward cancellation, necessary now that moves honor cancellation; each move retains its retry bound and every affected output gets a recovery attempt

The change omits a public restore-error type, cache-specific task-state rules, custom cancellation-cause plumbing, a ten-second total recovery cutoff, and rollback after successful final installation. Tests asserting those newly chosen policies did not establish defects in the reported case. Total recovery time depends on the number of outputs; a fixed total cutoff could abandon otherwise recoverable originals.

## Regression evidence

| Scenario | Evidence |
| --- | --- |
| Missing retained failure | Native CI above and `/tmp/devflow-cache-fix-engine-red.log`; a real invalid output parent also left the log empty before the engine change. |
| Missing publication stage | `/tmp/devflow-cache-fix-stage-red.log`; a missing staged artifact lacked the install stage and intended destination. |
| Retry behavior | Portable one-shot/open-reader/unlocked controls in `internal/fsutil/move_open_reader_test.go`; the reader closes only after the actual OS refuses the rename. The source uses no platform skip or timed release. |
| Preparation happens once | A controlled one-shot overlay fails the retry test in `/tmp/devflow-cache-fix-fsutil-one-shot-red.log`. A new destination appearing between attempts must survive. |
| Cancellation-safe rollback | `TestRestoreCancellationDuringLaterInstallRecoversAllOriginals` cancels before a later install, after earlier publication. Passing the canceled context into recovery fails this regression; recovery must still restore both original outputs. |
| Incomplete recovery | The rollback test fails recovery of one backup, verifies that the next backup is still recovered, and checks the exact retained backup path and bytes. |

Virtual-time tests cover backoff, the retry bound, nonretryable errors and cancellation while waiting. All shared scenarios remain part of `go test ./...` on the existing native CI matrix. Compiler success alone is not native filesystem verification.

Current full-suite, race, build and hosted outcomes are recorded in `PROGRESS.md` and [PR #22](https://github.com/benjaco/devflow/pull/22).

## Limits

A persistent reader or permission problem still fails after bounded retries. No additional service or unrelated process is terminated. Initial destination removal can fail before rename retry; failed recovery keeps backups and reports their location. This remains a transaction that recovers reported errors, not crash-atomic publication across multiple output paths.

Replacing a running `devflow-local.exe` is a separate executable-publication/lifecycle issue. A short cache-rename retry does not make an indefinitely running executable replaceable.
