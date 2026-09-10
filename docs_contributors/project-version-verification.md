# Project Version Selection Verification

The installed CLI previously generated its adapter module from the installed
version and parsed commands before bootstrap. A project's Devflow requirement
did not select the runtime. The following regressions captured semantic failures
before the corresponding corrections.

| Case | Observed failure | Corrected behavior |
| --- | --- | --- |
| Selected CLI implements a future command | Installed parser returned `invalid_arguments` before handoff | The selected module's own command receives unchanged arguments/environment |
| Devflow is linked as a module dependency | Version reporting returned `devel` | Report the linked Devflow version and effective replacement identity |
| Invalid project pin plus future command | Installed parser hid the invalid pin | Return `invalid_project_version` during bootstrap |
| Compact preparation failure | Summary/quiet request emitted an unbounded error without detail metadata | Retain compact error limits and quiet progress |
| Local source path contains spaces | Generated `replace` directive was invalid Go module syntax | Quote the source path using the Go module formatter |
| Instance points to another worktree | Runtime selection used the instance, but adapter bootstrap read the wrong cwd | Use the same instance precedence for both stages |
| A task invokes the global launcher in another pinned project | Inherited source/loaded markers bypassed the new project's selection | Bind the loaded marker to its executable and reserve the source override for explicit development |
| The same worktree is reached through a filesystem alias | Equivalent paths produced different runtime cache identities | Canonicalize the selected worktree and local source paths |
| Another adapter dependency requires a newer Devflow release | Go built an adapter with Devflow 1.3.0 despite the project's 1.2.3 pin | Verify the linked module and replacement before publishing or loading the adapter |

Local RED evidence is retained in:

- `/tmp/devflow-project-version-cli-red.log`
- `/tmp/devflow-project-runtime-version-red.log`
- `/tmp/devflow-project-handoff-red.log`
- `/tmp/devflow-project-review-red.log`
- `/tmp/devflow-project-version-instance-red.log`
- `/tmp/devflow-project-version-nested-red.log`
- `/tmp/devflow-projectversion-red.log` (initial semantic package stubs)
- `/tmp/devflow-projectversion-alias-red.log`
- `/tmp/devflow-projectversion-adapter-red.log`

The preparation package uses real Go builds against isolated local modules and a
local module proxy. These tests cover exact canonical/fork build identity,
checksum rejection without editing project files, cached use offline, concurrent
preparation, cancellation of real build children, source changes during builds,
bounded diagnostics, and corrupt cached executables. Source replacement tests
also cover embedded assets, beyond Go file changes, and filesystem aliases.
Real proxy fixtures verify matching canonical/fork/local adapter builds and reject
an adapter whose dependency graph raised Devflow's version above the project pin.

Compiled CLI fixtures cover bare startup, future commands and flags, worktree and
instance routing, argument-value/terminator handling, invocation environment,
quiet/states progress, cache reuse with Go absent, pin/source updates, no stale
execution after a failed build, adapter-independent recovery and version, and
explicit source overrides. A real adapter fixture runs a finite task, inspects
its instance from a different checkout, and reports its version despite a broken
adapter. Upgrade bypass tests reject an invalid option without installing anything.

Focused commands:

```bash
go test -race ./internal/projectversion ./internal/version -count=1
go test ./internal/cli -run 'TestProjectVersion|TestLocalAdapter|TestInvocationFlag|TestProjectPreparation|TestLocalProjectExecutionGuard' -count=1
```

Freeze Go sources and embedded user docs before compiled CLI checks: selected
source identities are checked again before publication. Final gates and any
standalone demonstration are recorded in `PROGRESS.md`. Local macOS execution
and cross-compilation do not establish native Windows/Linux runtime behavior.

## Final Local Verification

On macOS arm64 with Go 1.27.1, the full normal suite passed (CLI 116.398s),
followed by the full race suite (CLI 131.230s). Vet, Staticcheck v0.8.1,
govulncheck v1.6.0, tidy-diff, builds/examples, version JSON, formatting and diff
checks passed. Linux/Windows amd64 CLI/projectversion tests and CLI binaries
cross-compiled. Gate output is retained in `/tmp/devflow-pinned-final-*`.

`/tmp/devflow-pinned-demo.py` created an isolated project and temporary launcher,
then ran version, graph, doctor, `run verify --ci --json --progress quiet`, and
status before and after changing the requirement from `v0.1.0` to `v0.2.0`.
Each stage produced valid JSON and the expected task artifact. Two runtime cache
entries were selected, while the launcher and project module files were unchanged
by startup. Both demo requirements use the same explicit local source replacement;
this demonstrates handoff/invalidation, not two published releases. The proxy
tests above separately verify exact versioned-module selection.

Command records, stdout/stderr and `summary.json` are in
`/var/folders/g6/jbsk5f6s7ll59hy734gmngwc0000gq/T/devflow-pinned-demo-u97dtx8s`.
No global installation, remote workflow or existing development service was used.
