# Execution ownership: regression evidence

Scope: approved agent-verification item 1 and its current-only revision requested during review. Items 2–7 are queued for separate user review. Base commit: `a7fe4f8`.

Tests were added before the relevant fixes. Where concurrent implementation had already changed a dependency, temporary Go overlays ran the new tests against the base file without reverting shared work. Build failures for newly introduced lock APIs were followed by behavioral regressions against existing engine/CLI paths. The following failures were observed, rather than inferred from source.

Excerpt from the initial engine regression run:

```text
TestExecutionOwnershipRejectsBeforeMutation/ci_ci
competing execution error = <nil>; want resource_conflict
rejected contender invoked callbacks: configured=true executed=true
contender changed owner file status.json
contender changed owner file check.log
contender changed owner file artifact.txt
```

| Regression | Observed before the fix | Required behavior |
| --- | --- | --- |
| `TestExecutionOwnershipRejectsBeforeMutation` | CI/CI, watch/CI and CI/watch contenders returned success, invoked configuration/task callbacks, and changed `status.json`, `check.log`, and `artifact.txt` | Reject before configuration, preserving all captured owner files |
| `TestReplacementTimeoutPreservesActiveOwnerAndMetadata` | Replacement succeeded after the stop timeout, discarded the old active owner and rewrote instance metadata | Return conflict and preserve ownership/metadata |
| `TestConcurrentStartAdmitsOnlyOneReplacement` | Two concurrent starts entered the same stop/replace transition | Serialize the transition and start one matching replacement |
| `TestFailedDetachedEngineInitializationReleasesActiveSlot` | An engine-construction failure left a completed run in the active slot | Complete cleanup and release the slot |
| `TestExecutionOwnershipCleanupFailureDoesNotPermitReplacement` | Failed resource cleanup produced `success=true`; a replacement executed | Report failure and retain recovery evidence |
| `TestExecutionOwnershipChecksAliveAfterAcknowledgedStop` | A nil stop error was accepted even while the resource remained alive | Require confirmed exit |
| `TestLifecycleRestartKeepsOwnerWhenStopCannotConfirmExit` | A replacement generation started while the old resource remained alive | Preserve the old handle and reject restart |
| `TestExecutionOwnershipWatchScanFailureCleansResources` | Watcher setup failure left a registered resource running | Clean resources on every post-preparation exit |
| `TestCacheKeyOwnershipRejectsBeforeConfigurationAndFingerprintCallbacks` | Cache-key inspection invoked configuration and fingerprint callbacks during another execution | Admit before mutating preparation |
| `TestCacheKeyOwnershipRetainsResourceRegisteredByFailedFingerprint` | Failed fingerprinting did not clean its registered resource and allowed replacement | Clean or retain recovery evidence |
| CLI cache/daemon ownership tests | Cache invalidation removed entries/stamps despite contention; cache-key and daemon-admission errors emitted no JSON | Reject before mutation and emit structured ownership errors |
| Instance daemon-state tests | Recording a daemon rewrote runtime env; stale execution saves hid or resurrected daemon metadata; startup created execution state | Store authoritative daemon control separately |

Additional coverage verifies canonical aliases, independent worktrees, real child-process contention and owner death, corrupt-marker preservation, owner-only metadata, ownership retained through finalization, action cancellation/environment restoration, stale action relaunch, and failed-cleanup terminal events.

The initial full-suite pass also caught first-use actions missing execution state and invalid-graph preflight creating a lock directory. Independent recovery review added regressions for a daemon PID duplicated through process aliases, unresolved tasks without an owner marker, and confirmed stopped processes left in starting/restarting/degraded states. The initial implementation retained old supervisor/executor reconciliation; user review explicitly rejected that compatibility path, so the revision removes it and its obsolete tests.

Run the focused ownership tests:

```bash
go test ./internal/lock ./internal/execution ./internal/executionstate ./pkg/instance
go test ./pkg/engine -run 'Ownership|TestLifecycleRestartKeepsOwner' -count=1 -v
go test ./pkg/daemon -run 'ConcurrentStart|ReplacementTimeout|FailedDetachedEngine|ActionWait|CompletedAction|DaemonContention|Recovery' -count=1 -v
go test ./internal/cli -run 'Ownership|TestRunCIJSONReportsEnginePreflightFailureWithoutMutation' -count=1 -v
```

## Current-only revision

The current implementation uses `daemon.json` as the sole daemon control record, `daemon`/`daemonStarted` in JSON and `logs daemon`. There is no old supervisor/executor migration, process-table or launcher-log ownership discovery. Current APIs use `RequiredCLIs` and `OnServiceHandle`; migration-needed classification requires a typed error, and omitted validation details use the same `issues` default as JSON. Retired aliases are removed rather than forwarded.

These new tests were run before their fixes:

| Regression | Observed failure | After the fix |
| --- | --- | --- |
| `TestLoadDaemonDoesNotReadExecutionSnapshot` | An absent daemon record read corrupt execution JSON and failed | Independent daemon lookup succeeds with an empty record |
| `TestStopAllUsesRecordedTaskOwnershipOnly` | Cleanup inferred an executor PID from old launcher logs | Only explicit current task references enter the stop scope |
| `TestLoadSnapshotPreservesRecordedResourcesAfterDaemonExit` | TUI read deleted `daemon.json`, rewrote status and claimed live/PID-less resources stopped | Snapshot reads preserve all recorded state for explicit recovery |
| `TestDaemonLogsDoNotRequireExecutionSnapshot` | Daemon log access failed with missing or corrupt execution state | Diagnostics read the separate daemon record |
| `TestDefaultLaunchPlanDoesNotReadExecutionState` | Pure launch selection failed on corrupt execution state | Selection delegates live admission to the daemon |
| `TestTaskErrorClassificationRequiresTypedMigrationSignal` | Four ordinary error messages were classified as `migration_needed` by their prose | Only typed migration errors receive that state |
| `TestValidationDefaultsToIssuesDetails` | Omitted details selected exhaustive `full` output | API and JSON default to `issues`; exhaustive callers request `full` |
| `TestUpgradeClearsTaskCacheOnlyAfterSuccessfulInstall` | Successful upgrade kept old artifacts and omitted `cacheCleared` | Successful install clears task artifacts; failed install and unrelated state remain intact |

Upgrade fixtures use a compiled fake Go command and temporary `HOME`, `XDG_CACHE_HOME` and `LOCALAPPDATA`; no actual upgrade is performed. Cache cleanup is global and deliberately uncoordinated with running task-cache reads/writes, so upgrades should run between executions. Database volumes, worktree state and output files are outside that cleanup.

Run the revision regressions:

```bash
go test ./pkg/instance -run TestLoadDaemonDoesNotReadExecutionSnapshot -count=1 -v
go test ./pkg/daemon -run TestStopAllUsesRecordedTaskOwnershipOnly -count=1 -v
go test ./pkg/tui -run TestLoadSnapshotPreservesRecordedResourcesAfterDaemonExit -count=1 -v
go test ./pkg/engine -run TestTaskErrorClassificationRequiresTypedMigrationSignal -count=1 -v
go test ./pkg/validation -run TestValidationDefaultsToIssuesDetails -count=1 -v
go test ./internal/cli -run 'TestUpgrade|TestDefaultLaunchPlan|TestDaemonLogsDoNotRequireExecutionSnapshot' -count=1 -v
```

Final validation results are recorded in `PROGRESS.md`. Native Linux/Windows CI and opt-in Docker checks are separate from local macOS execution; cross-compilation does not prove OS runtime behavior. No existing user development services or external project files are used by these tests.
