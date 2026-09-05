# Validation lifecycle verification

Item 3 of the [approved sequence](agent-verification-plan.md), started on 2026-09-06 after the user accepted item 2 and its Windows CI correction at `f55be34`. Item 4 has not started.

## Observed red failures

The following regressions were run before changing runtime code. They use temporary worktrees, Go callbacks and portable filesystem/error fixtures.

| Scenario | Observed failure | Regression |
| --- | --- | --- |
| Hook supplies task environment | `Run` received no hook value in artifacts or orders | `TestValidationBeforeRunEnvironmentIsTaskLocal` |
| Hook fails before `Run` | Validation reported success and called `Run` once; hook error/log was absent | `TestValidationBeforeRunFailureStopsRunAndCapturesLog` |
| Hook-only producer feeds a dependency | Validation skipped the producer and reported its generated output missing | `TestValidationHookOnlyTaskProducesDependencyOutputs` |
| Hook writes an undeclared file | Artifact validation reported success with no issues | `TestArtifactValidationObservesUndeclaredHookWrites` |
| Task order depends on hook reads/writes | The valid `a → b` order failed because the hook output was missing | `TestOrderValidationIncludesHookReadsAndWrites` |
| Hook registers a supervised handle | Validation reported success and never stopped the handle | `TestValidationCleansHandlesRegisteredByBeforeRun` |
| Hook requests input | The hook was skipped, `Run` executed and validation reported success | `TestValidationRejectsPromptsFromBeforeRun` |
| CLI lifecycle in artifacts/orders/all | All nine hook-failure, hook-only and hook-env cases disagreed with the expected JSON/command result | `TestValidateLifecycleJSON` |

Additional guards cover failed-hook writes, cancellation, service/debug-service preflight rejection, hook/run errors combined with cleanup errors, a handle still alive after `Stop`, and cleanup continuing across multiple handles. These supplemental controls were first run after implementation; they are not claimed as observed red failures. Engine integration guards passed before extraction and protect hook runtime propagation through `Run`, `Ready` and `AfterReady`, instance-env isolation and cleanup after hook failure.

## Change and boundaries

`internal/taskexec.Run` now owns the callback sequence used by both engine and validation: clone the runtime/environment when a hook is present, invoke `BeforeRun`, and call optional `Run` only if the hook succeeds. The effective runtime is returned for engine service readiness. The old engine-local implementation is removed.

Validation skips only group tasks. Existing snapshots surround the entire callback sequence, so hook writes participate in artifact checks and order permutations, including writes made by a failing hook. Existing captured logs retain hook diagnostics. JSON uses its current task/order failure fields and issue kinds; no new flags or response envelope are introduced.

The engine retains cache/stamp decisions, scheduling, status and readiness. Validation retains its temporary worktrees, validation env, resource budgets, prompt rejection and finite-service restrictions. It attempts cleanup of every registered handle and joins stop/aliveness failures with the callback error. It does not provide isolation from databases, networks, absolute paths or unregistered resources, and it cannot force an arbitrary handle to stop. Hook environment isolation is not general isolation of adapter global state or mutations to shared instance metadata.

## Verification

```sh
go test ./pkg/validation -run 'TestValidation(BeforeRun|HookOnly|CleansHandles|RejectsPromptsFrom|RejectsServiceKind|Lifecycle)|TestArtifactValidationObservesUndeclaredHookWrites|TestOrderValidationIncludesHookReadsAndWrites' -count=1 -v
go test ./pkg/engine -run '^TestTaskLifecycle' -count=1 -v
go test ./internal/cli -run '^TestValidateLifecycleJSON$' -count=1 -v
```

Passed on Go 1.27.1 darwin/arm64: focused regressions, `go test -count=1 ./...`, `go test -race -count=1 ./...`, `go vet ./...`, Staticcheck v0.8.1, govulncheck v1.6.0 (no vulnerabilities), module/format/diff checks and version JSON. Both full suites include the examples. Engine, validation and CLI test binaries plus the CLI compile for Linux/Windows amd64. Native Linux/Windows execution remains a CI check; cross-compilation does not prove runtime behavior. `PROGRESS.md` records the review handoff.
