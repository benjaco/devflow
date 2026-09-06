# Metadata and verification planning evidence

Item 6 adds declaration-based inspection and an advisory verification planner.
Its original change left the five item 5 review findings open; those are now
addressed with item 7 in [compact evidence verification](compact-evidence-verification.md).

## Observed failures before implementation

The first focused public-command run used the existing implementation and the
new CLI tests. It failed as follows:

| Regression | Observed failure |
| --- | --- |
| `TestPlanJSONSelectsChecksWithoutRuntimeCallbacks` | `plan --files ... --json` returned `flag: help requested` instead of a selection. |
| `TestPlanJSONArgumentFailures/unknown_project` | An unknown project returned `invalid_arguments/parsing` from the missing command instead of `unknown_project/resolution`. |
| `TestGraphShowJSONIncludesSafeMetadata` | `graph show` retained target/closure but omitted the metadata projection. |
| Project/graph metadata API tests | Compilation failed on missing purpose/effect/resource types, builder methods, target verification and metadata projection. |
| Adapter-source classifier tests | Compilation failed on missing `IsSource`; matching then covered removed names without filesystem access. |
| Embedded-web-app declaration/planning tests | The adapter had no declared verification target and the initial planner stub returned no selections. |

The observed command was:

```sh
go test ./internal/cli -run 'TestPlanJSON|TestGraphShowJSONIncludesSafeMetadata' -count=1
```

Focused review then reproduced and fixed these additional failures:

| Regression | Observed failure |
| --- | --- |
| `TestMixedConfigurationReasonsAreDeterministic` | Mixed source/configuration changes lost ordinary selection reasons and allowed nondeterministic target deduplication. |
| `TestFileWriteReadConflictsRespectDependencyOrdering` | An unordered generated-file reader/writer pair omitted its resource conflict. |
| `TestSharedInputBranchesRequireCoverage` | A shared frontend input selected only the frontend check and claimed resolution while a backend branch still required its explicit target. |
| `TestFullTargetRetainsUntypedSourceReasons` | Mixed configuration/source changes reported `no_verification_check` for tasks without purposes even when the selected full target covered the source. |
| `TestPlanJSONEnrichmentErrorsLeaveResolvedFalse` | Corrupt ownership and invalid adapter sources preserved `resolved:true` alongside `success:false`. |
| `TestPlanTextShowsConflictsAndUncheckedPrerequisites` | Text output omitted the resolved state, unchecked prerequisite names and declared resource conflicts. |

## Windows rooted-path regression

[Windows CI run `34057692957` at `54a5ec5`](https://github.com/benjaco/devflow/actions/runs/34057692957/job/101552479071)
failed `TestScopeDigestDeterminismAndInvalidFiles` with `accepted "/absolute"`.
Windows `filepath.IsAbs` requires a volume, so the old check accepted a path
rooted in the current drive. The correction rejects a leading slash after native
separator normalization. Coverage also includes native rooted/UNC/traversal
paths, drive-relative paths, valid relative-path deduplication and the CLI's
`invalid_arguments/parsing` JSON. The expanded tests pass on macOS before the
fix; the recorded failure is native Windows CI, not a local reproduction.
Post-fix check results and the native Windows CI status are in `PROGRESS.md`.

## Contract coverage

The CLI regressions exercise narrow frontend selection, expanded frontend and
backend scope, one shared generator in the dependency closure, graph and change
scope identities, declared prerequisite names with unchecked availability, and
single-document typed parsing, resolution, ownership and source errors.
Separate cases distinguish ignored, unmatched and configuration paths, including
adapter companion additions, deletions and renames and excluded test/unrelated
Go files. Configuration changes select an explicit verification target and
include a finite artifact-validation command.

Forbidden callbacks count or panic on instance configuration, task execution,
hooks, service readiness and fingerprint evaluation. Environment values must be
absent from JSON. In-process planning must not create `.devflow` runtime state.
An existing execution lease must remain byte-for-byte unchanged, and malformed
owner evidence must survive a failed read with the computed plan retained.

Configuration identity uses the loader's exact validated source set and includes
source names and contents: companion changes alter it, excluded files do not,
and deleting an added companion restores the original digest. An absent source
marker produces explicit unknown configuration identity.

`TestBootstrapPlanLoadsAdapterWithoutRuntimeCallbacks` exercises actual adapter
compilation/loading from an explicit `--worktree` while invoked elsewhere. Its
runtime callbacks panic if reached; an unavailable CLI remains unchecked, and
planning creates no execution lock, owner, instance state or task logs.
Bootstrap compilation may create its own disposable build files.

## Focused verification

The default CLI plan/graph tests, focused CLI race tests and actual compiled
bootstrap plan test passed after Go source edits were frozen. Repeat with:

```sh
go test ./pkg/project ./pkg/graph ./pkg/planner -count=1
go test ./internal/cli -run 'TestPlanJSON|TestPlanText|TestGraphShowJSONIncludesSafeMetadata|TestBootstrapPlanLoadsAdapterWithoutRuntimeCallbacks' -count=1
go test -race ./internal/cli -run 'TestPlanJSON|TestPlanText|TestGraphShowJSONIncludesSafeMetadata' -count=1
```

Final full default/race suites (including examples), vet, Staticcheck v0.8.1,
govulncheck v1.6.0 (no vulnerabilities), formatting/module/diff checks and version
JSON pass. Linux and Windows amd64 affected test binaries and the CLI compile;
native Windows runtime behavior remains a CI check. The canonical status is in
`PROGRESS.md`.

These tests establish declared selection and absence of planner runtime
execution. Go adapter loading remains executable configuration. An owner
snapshot cannot establish active ownership, completed cleanup or future
admission, and matching declarations cannot prove complete test coverage.
Graph/configuration identity and requested path scope do not implement a saved
plan executor or prove which source bytes a future command will read.
