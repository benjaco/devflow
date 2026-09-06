# Agent verification: assessment and implementation plan

Status: all seven items approved for sequential implementation with user review after each item. Items 1–3 and their CI corrections are accepted; item 4 is implemented and locally verified, awaiting user review. See [ownership evidence](execution-ownership-verification.md), [freshness evidence](watch-freshness-verification.md), [lifecycle evidence](validation-lifecycle-verification.md), [CLI reliability evidence](cli-reliability-verification.md) and `PROGRESS.md`. Items 5–7 remain queued. The original assessment below was made on 2026-09-05 against `a7fe4f8`, one commit after the supplied review's `b46220f`; source references describe that baseline and may move. This does not establish which binary is installed in the external project.

User review policy: implement one item at a time and stop for review. Devflow is current-only: update current callers and docs directly, remove retired APIs/state readers, and clear disposable task artifacts after successful upgrades instead of maintaining migrations. Older adapter compilation is not guaranteed. This supersedes the compatibility-preservation assumptions in the original assessment.

The objective is an explainable path from changed files to appropriate checks and durable evidence. Correct execution ownership, input freshness, and lifecycle parity must underpin that path. A planner cannot compensate for incorrect execution or stale results.

## Disposition of the feedback

| Feedback | Verdict | What to change or reuse |
| --- | --- | --- |
| CI and watch share state/logs/resources; daemon transitions race | Valid, source-confirmed; race regressions still required | Protect execution ownership before allowing overlap. Separate run records protect evidence, but do not isolate files, environment, ports, databases, or services. |
| Startup edits can escape watch and produce a false-success flush | Valid, reproduced | Begin observation before the initial DAG and bind flush to processed changes in the current watch generation. |
| Validation skips `BeforeRun` | Valid, reproduced in artifact and order modes | Share the finite callback sequence and task-local environment handling; retain validation's sandbox and service restrictions. |
| Rich graph metadata and a verification planner | Useful product work, partly available | Extend existing graph impact, closure, prerequisite, description/tag, and action-effect facilities. Add explicit verification intent instead of guessing from task names. |
| JSON errors are inconsistent | Valid, reproduced for an unknown graph target; broader paths source-confirmed | The latest CI preflight fix is partial. Cover outer bootstrap, local CLI, resolution, daemon failures, and streaming output with documented current response shapes. |
| Unattended prompts and scoped cancellation | Valid, with an additional correctness issue | Reuse typed prompts and action inputs. Prompt IDs currently collide between subprocesses; fix identity and stale responses before exposing a public response API. |
| Compact/persistent results and resumable logs | Split: log bugs are reproduced; retention is valuable; compactness is optional | Fix the follower promptly. Build retained evidence on run/attempt identities. Add opt-in summary/progress controls; 12 KB alone does not justify a protocol redesign. |

The external adapter's reported `frontend/.next` overlap was not independently inspected. If both tasks use that directory, it is a resource conflict. The adapter should declare or separate it; core must not hardcode Next.js paths.

## Evidence and current boundaries

Source references below refer to the assessed commit and may move during implementation.

- Ownership: direct CI uses the same instance preparation and persistence as watch (`pkg/engine/engine.go:384,434`), status (`pkg/instance/instance.go:96`), and task logs (`:212`), which are truncated at attempts (`pkg/engine/engine.go:923`). Atomic replacement prevents torn state, not competing owners overwriting each other.
- Daemon transitions: `Serve` handles connections concurrently (`pkg/daemon/daemon.go:267`); `startActive` separates check/stop/replacement and ignores stop timeout (`:993,1010`). Attached execution and invalidate/relaunch have related paths (`:1091,2009`). `daemon.Ensure` protects daemon startup; `LifecycleController` protects service transitions within one engine. Neither serializes all engine ownership changes.
- Freshness: the first DAG runs at `pkg/engine/engine.go:163`; the watcher baseline starts at `:196` / `pkg/watch/watch.go:102`. Flush accepts existing done/cached states (`pkg/engine/engine.go:2042`).
- Lifecycle parity: normal execution clones hook environment and invokes `BeforeRun` then optional `Run` (`pkg/engine/engine.go:900`). Validation returns immediately for hook-only tasks and otherwise calls `Run` directly (`pkg/validation/validation.go:342`).
- Metadata: `project.Task` already has descriptions, tags, inputs, outputs, dependencies, cache/stamp and restart settings (`pkg/project/project.go:72`). Actions already have input schemas and effects (`pkg/project/actions.go:29,42`). `graph show` exports only target/closure (`internal/cli/app.go:2090`); `graph affected` already exports direct/downstream matches and explanations (`:2130`).
- Adapter discovery: bootstrap includes the entrypoint plus sorted regular root-level `devflow_*.go`, excluding `_test.go` (`internal/cli/local_project.go:155`). Ordinary input matching does not classify those as configuration changes.
- Protocol: bootstrap precedes dispatch (`internal/cli/app.go:44`); unknown graph targets return before JSON (`:2119`). Attached runs can discard an available failed daemon result on transport error (`:608`). Attached watch JSON also writes a plain `watching ...` line (`:693`).
- Input: JSON actions suppress streamed events (`internal/cli/app.go:1141`); prompts wait for answer files (`pkg/engine/engine.go:989`). Subprocess prompt IDs restart at `prompt-1` (`pkg/process/process.go:464`), while answer paths use only instance and prompt ID (`pkg/instance/instance.go:295`).
- Cancellation: direct CI uses `context.Background()` (`internal/cli/app.go:463`). Client socket cancellation exists, but server operations receive the daemon lifetime context (`pkg/daemon/daemon.go:683`). Ending a client wait does not necessarily cancel its operation.
- Evidence: `RunResult` lacks general run identity (`pkg/api/types.go:272`). Action results have a run ID, and service nodes have attempts/generations, but neither is a retained identity for all executions and finite-task attempts. Current status and logs are overwritten.
- Logs: `logsCmd` prints a tail (`internal/cli/app.go:1515`) then follows from offset zero (`:2486`); shrinking logs never reset the offset. The follower also advances to a pre-read stat size, mishandles partial/blank lines, and lacks cancellation. CLI tailing reads the whole file; the TUI already has bounded reading logic that can inform a shared reader.

Isolated Go-overlay tests reproduced these outcomes without modifying runtime source or starting existing services:

1. Pause startup after reading `old`; write `updated`; release; wait for watcher readiness; flush. Actual: `synced=true`, `success=true`, one execution, artifact still `old`.
2. In artifact and order validation, hook-provided environment is absent, a failing hook permits success, and hook-only tasks report missing outputs. Artifact validation also misses an undeclared hook write. Order validation is not intended to identify arbitrary undeclared writes.
3. `graph show missing-target --json` returns an error with zero stdout bytes.
4. Tail returns `old-b`, then following replays `old-a` and `old-b`. Truncating a 30-byte log to `new\n` produces no new line after multiple polling intervals.

These are defect reproductions, not claims that the repository's existing test suite failed. Ownership races and prompt collisions remain source-confirmed until deterministic regression fixtures are added.

## What agents can already do

| Need | Existing path | Limit |
| --- | --- | --- |
| Explain changed-file impact | `devflow graph affected --files <paths> --explain --json` | Gives direct/downstream tasks, not a semantic recommendation of which checks establish coverage. |
| Inspect execution closure | `devflow graph show <target> --json` | Ordered names only; definitions still require adapter reading. |
| Check prerequisites | `devflow doctor --target <target> --strict --json`; `clis status --target` | Reuse target-scoped CLI/env selection; do not invent a second prerequisite resolver. |
| Run a focused check | `devflow run <task-or-target> --ci --json` | Use a separate worktree containing the edits when a watcher may conflict. Flushing first does not make later same-worktree CI safe. |
| Synchronize an existing dev environment | `watch --detach`, then `flush --json` | Item 2 establishes observation before startup and reconciles through a fresh scan before acknowledgment. Require `success=true`; restart-policy blocks need explicit rerun/restart. Changes after the final scan need another flush. |
| Validate adapter input/output/dependency declarations | `devflow validate <finite-target> --mode artifacts` or bounded `--mode all --details issues --json` | Specialized repeated sandbox execution; not a substitute for everyday checks or external-resource isolation. |
| Discover explicit actions | `devflow action list --json`, action input flags, TUI prompts | Input schemas/effects already exist. Unexpected interactive questions still lack an unattended response protocol. |
| Inspect failures and preserve immediate evidence | Save final stdout separately from stderr; inspect failure excerpts/node results before requesting logs | `jq` can reduce context now, but cannot reconstruct overwritten attempts or guarantee durable history. |
| Preview service operations | `restart --preview`, `stop --task ... --preview` | Describes lifecycle effects, not verification coverage or arbitrary CI conflicts. |

A separate worktree must contain the changes being tested, including relevant untracked files. A new checkout of unchanged HEAD does not verify the agent's edits. Worktrees isolate local files and managed instances; a shared external database still needs explicit adapter isolation.

## Implementation sequence

Deliver each item as a separate reviewable change. Use independent subtasks within the active item, then stop for user review before starting the next item. Fix correctness before building the planner.

### 1. Enforce execution ownership

Reason: this prevents destructive interference and invalid evidence, including interactions that distinct log files cannot solve.

- Serialize daemon check/stop/replace/retarget/invalidate/action ownership transitions with a dedicated transition mechanism and generation/reservation token. Do not hold the existing state mutex while waiting for shutdown, or hold a transition lock for the entire foreground action; status, cancellation, and prompt responses must remain usable.
- If shutdown times out, retain ownership and reject replacement with a typed busy/timeout result. An action's deferred relaunch must not overwrite a newer user's selection.
- Add a cross-process worktree execution lease shared by direct CI and daemon engines, acquired before instance/env/port/status/log mutation and retained until cleanup is confirmed. Initially reject any competing executor with `resource_conflict`, naming the active owner and target. Do not start a daemon just to run CI.
- Cover daemon `recordRun`, temporary environment changes and provisioning before engine entry, not just `Engine.Run/Watch`. Audit `Ensure/Serve` control-metadata writes too: separate daemon transport metadata from execution snapshots or coordinate those writes through the admission boundary. Otherwise even a rejected launch can overwrite the current owner's state.
- Keep existing DAG task parallelism inside an execution. This initial guard is deliberately conservative about simultaneous independent runs.
- Recover abandoned leases through OS locking plus recorded-resource reconciliation. Owner process exit alone does not prove its child processes or managed containers stopped. Uncooperative Go callbacks cannot be safely killed in-process; report incomplete cleanup and keep resources busy.

Acceptance: concurrent same/different-target starts; CI/watch and CI/CI contention; stop timeout; action/relaunch races; recovery after owner death; another worktree remains independent. Every rejected request leaves the existing status, logs, env, outputs and services unchanged. Use barriers and real cross-process fixtures, not timing-only assertions.

Primary owners: `pkg/daemon`, `pkg/engine`, `pkg/instance`, `internal/lock`, `pkg/api`.

### 2. Close the startup freshness gap

Reason: a successful flush must not bless an artifact produced from inputs that changed during startup.

- Establish the scoped observer before initial task execution. Coalesce pending changes during startup and subsequent rebuilds without dropping invalidations or deadlocking on a full batch channel.
- Separate observer establishment from target health. Startup failure must permit recovery after edits, while flush remains unsuccessful until the selected target is healthy.
- Bind flush requests/acks to the watch generation and a processed input-observation boundary. Reconcile pending selected-input changes before acknowledging that boundary; a sentinel alone is insufficient.
- Respect filtered input semantics, generated-output handling, `RestartNever`, and warmups excluded from watch. If policy prevents required refresh, report an explicit unsettled/manual-action issue rather than falsely declaring freshness or silently overriding policy.
- Cancel the observer and clean up registered resources on every exit. Avoid blanket suppression of startup output directories that could swallow real source edits.

Acceptance: the paused-startup reproduction produces the updated artifact before flush succeeds; edits during rebuild; concurrent flush boundaries; initial failure then repair; scanner failure; cancellation/full queue; generated artifacts do not loop; adjacent source edits survive suppression; old acknowledgments cannot satisfy a replacement watch.

Contract limit: flush synchronizes declared inputs and eligible work in a particular watch generation. It does not run undeclared tests, prove that all possible inputs were declared, or promise observation of every transient metadata-preserving edit. If strict byte-level freshness is required, reconcile the selected input fingerprints explicitly rather than relying on polling metadata; never hash the whole repository by default.

Primary owners: `pkg/watch`, `pkg/engine`, flush state in `pkg/instance`, `pkg/api`.

### 3. Share finite task lifecycle with validation

Reason: validation must inspect the behavior the engine actually executes, including hooks that prepare environment, write files, or fail.

- Extract a small internal callback helper for task-local environment cloning, `BeforeRun`, and optional `Run`, returning the effective runtime and error. A package such as `internal/taskexec` can serve engine and validation without making either depend on the other's orchestration.
- Retain engine ownership of caching, log attempts, status and readiness. Retain validation's sandbox, no-cache/no-stamp behavior, validation env, prompt/service rejection, resource budgets, and registered-handle cleanup.
- Do not skip hook-only tasks. Account for hook writes in artifact snapshots, hook failures in both modes, and cleanup after a hook registers a handle and fails.

Acceptance: hook env reaches `Run` without leaking to other tasks; hook errors prevent `Run`; undeclared hook writes are reported in artifacts mode; hook-only outputs succeed; order permutations include hook effects; illegal handles are stopped. `AfterReady` parity is unnecessary because service closures remain unsupported in validation.

Primary owners: shared internal helper, `pkg/engine`, `pkg/validation`.

### 4. Repair immediate log and error-path defects

Reason: agents cannot diagnose or recover operations if error output disappears or logs replay and omit evidence.

- Implement bounded tail/follow reading with one consistent handoff offset, actual consumed-byte offsets, cancellation, writer-error propagation, partial-line buffering, and truncation/replacement detection. Reuse or extract existing bounded log-reading machinery where its contract fits; do not import the TUI into CLI.
- Introduce typed error classification and a shared CLI presentation boundary for code, phase and message. Use one documented current error contract while retaining useful command-specific result data. Do not add aliases or envelopes solely for retired consumers.
- Start with actionable codes such as `invalid_arguments`, `unknown_project`, `unknown_target`, `adapter_compile_failed`, `resource_conflict`, `interaction_required`, `deadline_exceeded` and `operation_cancelled`, with phases identifying parsing, bootstrap, resolution, admission, execution or transport. Classify at the source; do not derive codes by parsing error prose.
- Cover argument parsing, project/target resolution, adapter discovery/compilation, transport and execution. Recognize JSON mode at the outer entry boundary, including malformed commands, without mistaking argument values or tokens after `--` for flags.
- Preserve available failed daemon results. Avoid double error documents across parent/bootstrap/local binaries. Treat finite one-result JSON and streaming JSONL separately; remove plain text from JSONL watch output.
- Add direct CLI signal propagation promptly, including bootstrap child execution and repository work; reuse existing signal-aware validation patterns. This fixes local cancellation while the run-scoped server protocol in step 5 handles daemon operations.

Acceptance: tail/follow has no replay; append during reading; shrink/replacement; partial UTF-8 and blank lines; large logs remain bounded; writer failure and cancellation. Compiled CLI tests cover unknown target/project, invalid flags with JSON in different positions, missing/broken adapters, daemon failures, and task failures through direct and attached paths. Exactly one finite result or a valid JSONL stream, with correct nonzero exit status.

Primary owners: `internal/cli`, bootstrap entrypoints, shared log/error helpers, `pkg/api`.

### 5. Establish run identity, retained evidence and unattended control

Reason: this supplies a stable answer to which attempt passed, which operation is waiting, and what an agent may cancel or answer.

- Assign one run ID before execution and a distinct ID for every task attempt, including finite tasks. Reuse/generalize the existing action run ID and service generation concepts instead of introducing competing identity systems.
- Propagate identity through events, logs, prompts, results and cancellation. Store owner-only bounded run records and append-only attempt logs under the instance, with atomic terminal results and an explicit retention policy. Keep instance status as the active development view; finite results must not replace a live watcher's display.
- Retain enough provenance to identify target, graph/adapter version, selected input fingerprints already computed during execution, timestamps, per-attempt outcome, cache reuse and failure details. Distinguish executed checks from cached/skipped work. A passing result remains evidence of that run, not automatically of later edits.
- Expose read-only run result/list retrieval and run-scoped cancellation. Proposed surface: `runs list/show/cancel`, plus `logs --run`; exact naming should follow the existing CLI style. End-of-wait cancellation for `flush`/status must not stop the user's watcher. Detached execution survives an observer disconnect; an explicit operation cancellation stops only its owned resources.
- Fix prompt identity even within a single parallel DAG: run + task + attempt + prompt identity, with rejection of stale, duplicate, expired or mismatched responses. Persist pending prompt metadata and expose list/respond commands. Never retain secret answers in ordinary logs/results.
- Define headless behavior explicitly: `fail` finishes with `interaction_required` and cleanup; intentional `wait` retains a waiting operation that can be inspected/responded to until its deadline. A failed operation's diagnostic prompt is not still answerable. No blanket automatic confirmation; known action inputs remain the first choice.
- Propagate operation deadlines to execution, not just socket waits; use a bounded cleanup context. Keep cancel/respond/status responsive while a foreground action is waiting.

Acceptance: historical results survive new runs/restarts; attempts do not overwrite each other's logs; pending prompts survive reconnect; identical prompts in parallel tasks and stale answers after retries are isolated; invalid answer types reject; headless failure/wait policies; cancellation while queued/running/waiting; unrelated development services survive. Retention removes only eligible completed evidence and reports expired IDs/cursors explicitly.

Primary owners: `pkg/api`, `pkg/instance`, engine/daemon execution contexts, `pkg/process`, CLI/TUI adapters.

### 6. Expose metadata and build a conservative verification planner

Reason: agents should select checks from declared intent and graph structure without rereading Go adapters or guessing task names.

- First add a stable, serializable task/target metadata projection to graph inspection. Keep useful closure/name fields in the current projection. Expose descriptions, tags, dependencies, inputs/outputs, cache/stamp/restart policy, prerequisite names, and hook/readiness presence. Do not serialize Go callbacks or environment values.
- Add explicit purposes such as test, lint, typecheck, build, format and generate, plus adapter-defined verification targets and generic effects/resource declarations. Existing tags may supply explicit purposes; names alone cannot establish purpose. Generalize action effects without treating artifact `Outputs` as a complete side-effect declaration. Unknown effects remain unknown.
- Implement a pure planner over the current graph: changed files -> direct/downstream impact -> explicitly eligible verification checks -> deduplicated dependency closure. Return selection reasons, shared dependencies, declared prerequisites/effects, unresolved files/metadata, resource conflicts and any prerequisite synchronization.
- Proposed CLI: `devflow plan --files frontend/src/example.tsx --intent verify --json`. Verify selects checks; authoring/migration/reset/format actions are not selected merely because they are downstream. Required generators remain visible in the dependency closure with their declared writes.
- Reuse prerequisite selection, path matching, filtered/ignored semantics and topological ordering. Plan without running tasks, hooks, readiness, remote fingerprints or instance provisioning. Prerequisite availability can remain unknown until existing doctor checks run. Go adapter loading itself is executable configuration, not a security sandbox.
- Share bootstrap's exact adapter-source classification. Classify added, deleted and renamed entry/companion files as configuration changes, not ordinary unmatched source files; `_test.go` and unrelated root Go files retain their actual loader meaning. A configuration change means ordinary impact analysis is incomplete. Prefer an explicit full verification target and finite adapter validation; otherwise report unresolved coverage, never invent a narrow safe subset.
- Keep the initial planner advisory: return explicit task/target commands for existing execution APIs, plus graph/config digest and requested change scope for evidence. Defer a saved-plan executor. If later added, it must revalidate the complete change set/source snapshot as well as graph/config: additional source edits can require more checks without changing the graph. Consumed fingerprints show what selected checks read, not whether selection still covers every edit. Matching depends on declared coverage, not a claim to prove that the test suite is complete.

Acceptance: frontend/backend/shared-input changes; shared dependencies appear once in the proposed closure; required env/CLIs deduplicate; format/authoring effects are explicit and excluded as verification goals; unknown metadata yields uncertainty; ignored versus unmatched paths differ; adapter additions/deletions/renames invalidate the plan; extra source edits change the recorded scope; planner invokes no task/environment-provisioning callbacks. Validate one real adapter against expected check selections, then publish generic examples. Before any future saved-plan executor ships, test stale graph and stale change-scope rejection plus single execution of shared dependencies.

Primary owners: `pkg/project`, `pkg/graph`, a small pure planning package, adapter-source discovery helper, `pkg/api`, CLI and example adapters.

### 7. Add compact views and resumable retrieval on those contracts

Reason: reduce agent context while retaining enough evidence to recover after retries and context loss.

- Add opt-in `--details summary|issues|full` for run/status/flush; choose and document defaults for current consumers. Reuse validation's exact counts, bounded samples and truncation semantics. Summary must keep success, run identity, sync/freshness state, actionable failures and evidence references.
- Add independent progress control such as `--progress quiet|states|logs`. This matters even when stdout is JSON because many agent tools combine stderr and stdout. Do not hide the final failure result.
- Add cursors bound to run/task/attempt and byte position or log generation. Return explicit reset/expired responses rather than reusing an offset in another attempt. Immutable attempt logs make this simpler than guessing solely from file size. Cross-attempt following must announce a switch.
- Bound retention and read sizes; avoid a database or event-sourcing architecture for local run evidence.

Acceptance: large healthy graph produces bounded requested summaries; failures remain discoverable; documented full/default contracts; quiet progress stays quiet; cursor pagination has no gaps/replay for retained attempts; partial lines, new attempts, replacement and expiry are explicit.

## Defer or avoid

- A general same-worktree concurrent resource scheduler is not required for the first release. Use conservative conflicts first; later permit overlap only for sufficiently declared resources/effects, including service lifetime, cache restores, installers and databases. Record real rejected workflows to justify that complexity.
- Run IDs are not sandboxes. Do not advertise concurrency safety from separate status/log directories alone, and do not auto-stop the user's watcher to make a verification command fit.
- Do not infer test coverage from names, treat unmatched files as proven irrelevant, or treat missing outputs/effects as proof of read-only behavior.
- Do not run exhaustive order validation after every source edit. Reserve it for finite adapter changes and deliberate pipeline hardening within its combinatorial/resource limits.
- Do not replace project policy in `AGENTS.md` with tool guesses. Devflow should expose mechanics/evidence; adapters and repository policy still decide which checks are required and which external effects are allowed.
- Do not start with MCP, a new config DSL, broad framework-specific detection, universal success-envelope changes, or storage optimization. An MCP wrapper should be a thin consumer of the completed CLI contracts.

## Delivery and verification

Review item 3 lifecycle parity next, then deliver item 4 log/error-path fixes separately. Continue with identity/input/error contracts, metadata/planning and compact retrieval in the sequence above; stop after each approved item for review.

Each change needs its targeted failing regression, subsystem docs, current JSON contract coverage where relevant, examples that still build, `go test ./...`, `go test -race ./...`, vet and the repository quality gates. Cross-process ownership/cancellation/log behavior needs native Linux/macOS/Windows CI; compilation alone cannot prove those behaviors. Use opt-in Docker coverage when resource ownership affects managed containers and a real daemon/TUI smoke for action interruption and prompt recovery.

Planning verification here used source inspection and isolated temporary Go-overlay probes only. No runtime fixes, installs, service restarts or changes to the external project were made.
