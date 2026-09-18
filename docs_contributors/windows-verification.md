# Native Windows verification

This audit runs on Windows amd64 with Go 1.27.1, Delve 1.27.1, Node
22.22.2/npm 10.9.7, PostgreSQL 16 clients and Docker Desktop (Engine 29.4.1).
It uses disposable worktrees, containers and volumes. It does not exercise a
consumer project's database or change machine security settings.

## Reproductions and corrections

- The first Go invocation failed before testing: the downloaded Go 1.27.1
  toolchain contained only its `bin` directory. Moving that incomplete directory
  aside let Go reconstruct it from its cached archive. This was local setup,
  not a repository failure.
- The unrestricted cold build exhausted Windows commit memory in several
  linkers. Subsequent runs use process-local `GOFLAGS=-p=2` and
  `GOMAXPROCS=4`, including nested fixture builds. No global Go configuration
  or pagefile setting was changed.
- Git's `core.autocrlf=true` made `gofmt` report untouched Go files and
  `go mod tidy -diff` report every checksum line. Repository attributes now keep
  Go sources, module metadata and the shell launcher as LF on every host.
- The Payload terminal test's deleted-field edit matched an LF-terminated line.
  A CRLF checkout left the field unchanged and timed out waiting for a warning.
  Match the declaration independently of line endings and require an actual edit.
- Extending the real TUI test to create an initial migration exposed loss of the
  watcher's prompt policy after an action. The subsequent development rename
  failed headlessly. Relaunch now preserves the previous live watcher's policy,
  while starting a new run without the action's deadline/cancellation identity.
  Portable daemon tests cover both wait and fail policies with the opposite
  policy on the action, operator response and cancellation after action completion.
- Native PostgreSQL `psql` ignored options following the positional database URL
  and waited instead of restoring the dump. Both host clone and SQL-file migration
  commands now use explicit `-d`, with options first; `-X -w` disables ambient
  startup scripts and password prompting. A stricter portable fake reproduces
  both failures; the real clone passes after correction.
- Publishing a complete run directory failed with Windows access denied.
  Publication now uses the existing bounded, cancellation-aware rename retry
  policy without deleting the destination or changing permissions. A portable
  real-open-directory regression checks publication after the reader closes.
- Upgrade streaming fixtures used a 500 ms child delay and a short startup
  timeout. A release-file handshake now keeps the child alive until streaming
  is observed, and cleanup cancels and joins it before restoring test environment.
- The fake Payload watcher's initial readiness and the Git/process cancellation helpers
  also exceeded their startup watchdogs under suite load. They now allow 30 seconds
  for startup, retain the existing edit/cancellation timing assertions, and report
  startup evidence or join child cleanup on failure.
- The race run reproduced the oversized-output scanner fixture timing out before
  reporting its scanner error. Its helper now emits readiness before the oversized
  line: startup has a separate watchdog and output drainage retains its original
  ten-second bound. Three race repetitions pass, including a slower startup;
  production output handling is unchanged.
- A native bootstrap fixture saw a transient sharing error reopening its published
  daemon-PID marker. Its readiness helper now waits for readable, nonempty bytes
  and returns that read; cleanup also joins the observer. The actual independent
  daemon ownership and forced-cancellation assertions are unchanged.
- A fake-debugger watch fixture timed out during startup and skipped its
  success-only cancellation, leaving the execution lease open during `TempDir`
  removal. It now always cancels and joins before restoring the test environment.
  Its startup watchdog is 30 seconds; real Delve CI probes allow a minute for
  cold debugger startup, retaining their readiness, restart and PID-cleanup
  assertions. Production debugger timeout policy is unchanged.

## Skip audit

Twelve unconditional Windows exclusions were removed:

| Area | Coverage now attempted on Windows |
| --- | --- |
| Filesystem copying | Internal pnpm directory symlinks and rejection of external relative links |
| Task cache | Symlink ancestors, corrupt linked artifacts, pnpm snapshot/restore |
| Command output cleanup | Symlinked output directories and hash-file parents |
| Worktree dotenv | Linked-worktree aliases and broken local dotenv links |
| Builder commands | Port references resolved into subprocess environment |
| Database sources | Adapter/database env merging and nonduplicated process logs |
| Prisma | Default migration command arguments |

Symlink creation skips only the actual Windows missing-privilege error in these
fixtures. It does not skip based on OS name or hide unrelated filesystem errors.
All these cases execute on this host. Unix directory-mode assertions remain
conditional inside otherwise portable pnpm coverage.

The six remaining unconditional Windows skips test Unix directory permission
semantics (four), colon output filenames (one), and newline directory names
(one). These do not represent supported Windows filesystem contracts. Logical
task names containing colons still run on Windows. Existing capability-based
Git, Delve, symlink, permission-enforcement and console-control gates were
reviewed; installed dependencies and native console support allow their relevant
cases to run here. The Unix FIFO adapter test and Unix signal-delivery test keep
their build tags; Windows has separate child-exit, CTRL_BREAK and daemon-console
isolation tests. The TUI terminal build tags select platform setup helpers, not
an E2E exclusion.

## Commands and environment

The only user-facing test opt-ins found are `DEVFLOW_E2E_PAYLOAD=1` and
`DEVFLOW_E2E_DOCKER=1`. Other `DEVFLOW_*TEST*`, `DEVFLOW_FAKE_*` and
prompt-child switches belong to individual subprocess fixtures; do not export
them for a suite. Avoid `-short` when requesting Docker coverage. Explicitly
enabled Docker tests now fail when Engine/client prerequisites are unavailable.

```powershell
$env:DEVFLOW_E2E_PAYLOAD = '1'
$env:DEVFLOW_E2E_DOCKER = '1'
$env:GOFLAGS = '-p=2'
$env:GOMAXPROCS = '4'
go test -count=1 -timeout=30m ./...
go vet ./...
go run ./cmd/devflow version --json
```

For source development, build a reusable native binary and select this checkout
for adapter compilation, just as the root shell launcher does:

```powershell
$devflowSource = (Get-Location).Path
go build -o .devflow/bin/devflow.exe ./cmd/devflow
$env:DEVFLOW_BOOTSTRAP_ROOT = $devflowSource
$env:DEVFLOW_BOOTSTRAP_ENTRY = '1'
& "$devflowSource/.devflow/bin/devflow.exe" version --json
# From the target worktree, invoke the same executable for doctor/run/status.
```

The root `devflow` launcher is a shell script. These bootstrap variables are
source-development settings for the current PowerShell session, not test opt-ins.
Use a fresh session for the globally installed release or remove both variables.

[Go's Windows race detector](https://go.dev/doc/articles/race_detector#Requirements)
requires cgo and a compatible MinGW-w64 compiler. This audit uses the portable
[LLVM-MinGW 20260908 release](https://github.com/mstorsjo/llvm-mingw/releases/tag/20260908),
verified against its release SHA-256, under ignored `local/windows-audit/tools`.
Set `CGO_ENABLED=1`, `CC` to its `bin/gcc.exe`, and prepend its `bin` to
the current process's PATH before `go test -race`. This is test tooling, not a
Devflow runtime dependency.

After unpacking that release into the ignored audit tooling directory:

```powershell
$env:CGO_ENABLED = '1'
$env:CC = (Resolve-Path 'local/windows-audit/tools/llvm-mingw-20260908-ucrt-x86_64/bin/gcc.exe').Path
$env:PATH = (Split-Path $env:CC) + [IO.Path]::PathSeparator + $env:PATH
go test -race -count=1 -timeout=30m ./...
```

## Evidence and limits

Logs are retained under ignored `local/windows-audit/`: `baseline.jsonl`,
`psql-options-{red,green}.log`, `payload-tui-crlf.log`,
`payload-tui-migrations.log`, `action-policy-{red,green}.log`,
`portable-skips.log`, `focused-fixes.log` and `race-terminal.log`.
The baseline's late fake-psql failure also reflects the deliberately stricter
regression fixture being introduced during the audit; its isolated red/green
runs are the authoritative argument-parsing evidence.

Final validation covers all 32 packages with tests. Results combine full-suite
runs and corrected-package reruns; the full-run logs deliberately retain the
failures that prompted the final fixture corrections.

| Check | Result and evidence under `local/windows-audit/` |
| --- | --- |
| Normal tests, both E2E flags | All packages verified after corrections: `normal-complete.jsonl`, `normal-cli-complete.jsonl`, `normal-engine-complete.log` |
| Race tests, both E2E flags | All packages verified after corrections, no reported races: `race-final.jsonl`, `race-process-complete.log`, `race-engine-complete.log`; affected bootstrap cases also pass five race repetitions in `bootstrap-readiness.log` |
| Real Payload workflows | Engine and expanded ConPTY/TUI E2Es pass in both full-suite runs; migration SQL, preserved rows, repeated questions and owned shutdown are asserted |
| Docker integrations | Managed service, snapshot/restore, native host clone and PostGIS 16/17/18 cases pass in normal/race runs |
| Remaining exclusions | Exactly six Windows-inapplicable filesystem tests skip; all enabled integration and available capability tests execute |
| Quality/build | Vet, Staticcheck v0.8.1, Go 1.27.1 formatting, tidy-diff, CLI/example builds, version JSON and diff checks pass; govulncheck v1.6.0 reports no vulnerabilities. See `quality-final.json`, `tidy-final.log`, `govulncheck.log` and `version-final.json` |

The branch was subsequently rebased onto `origin/main` at `c772a61`, preserving
the Windows changes and the upstream repository-repair path diagnostics. Only
`PROGRESS.md` required manual conflict resolution. The full-suite evidence above
predates that rebase; follow-up checks cover the complete repository-repair
package and all CLI repository-repair/upgrade-streaming cases under the race
detector, with both E2E flags set. All pass, along with affected-package vet,
CLI/example builds, version JSON and formatting/diff checks. Evidence is in
`rebase-reporepair-race.log`, `rebase-cli-race.log`, `rebase-quality.json` and
`rebase-version.json`; `rebase-preservation.json` records the preserved file set.

The final native CLI is `.devflow/bin/devflow.exe`. The implementation ledger is
[`PROGRESS.md`](../PROGRESS.md). Native ConPTY input/output and SQL assertions do not establish
clipboard behavior, pixel fidelity or the historical unexplained IDE-terminal
exit. No claim is made about those manual terminal-host cases.

## Windows CI deadline and watch-start follow-up

[Windows job 105315689513](https://github.com/benjaco/devflow/actions/runs/35254804434/job/105315689513)
on `95819ec` failed only `TestRunRequestDeadlineCancelsExecution/attached` and
`TestWatchGeneratedOutputDoesNotHideSiblingSourceEdits/paths`. All other jobs,
including Linux race, macOS, and both native Linux Payload/TUI Docker jobs,
passed. The unchanged failing tests each pass ten local Windows repetitions;
the hosted failures were at task/watch startup, before source-edit assertions.

The deadline fixture assigned 500 ms to the entire operation, including durable
run setup, then required the task to start. It now uses `testing/synctest` and a
task-start barrier, checks the exact context deadline, verifies no cancellation
immediately before expiry, and requires `context.DeadlineExceeded` at expiry.
Both attached and detached cases verify the durable canceled run and error code.
Detached execution is joined beyond its admission response, with a bounded wait
for final evidence writes. Cleanup cancels and joins the request, including
failed assertions. Queued deadline coverage remains separate.

The watch-policy fixture now allows 30 seconds for initial setup while retaining
the four-second flush watchdog and exact generation/service counts. Readiness
failures include the marker error and current task status at the caller's line.
A temporary five-second first-generation delay reproduces failure with the old
four-second startup watchdog and passes with the corrected fixture, including
all subsequent sibling-edit and no-extra-restart assertions. The injected delay
was removed after this check. Production execution/deadline behavior is unchanged.

Evidence under ignored `local/windows-audit/`: `deadline-watch-ci.log`,
`deadline-watch-ci-jobs.jsonl`, `deadline-watch-baseline.log`, `deadline-clock.log`
and `watch-startup-delayed-{red,green}.log`. Both reported tests pass five race
repetitions (`deadline-watch-race.log`); the final running-deadline and queued-expiry
cases pass ten race repetitions (`deadline-final-race.log`). Complete daemon and
engine normal suites pass (`deadline-watch-packages.jsonl`), followed by the
complete daemon suite after the explicit execution join (`deadline-daemon-final.log`).
All these runs set both E2E flags. Affected-package
vet, formatting and diff checks pass (`deadline-watch-quality.json`). The follow-up
changes only fixtures and documentation; a hosted Windows run of the correction
remains pending.
