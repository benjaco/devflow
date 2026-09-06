# CLI reliability verification

Item 4 of the approved agent-verification plan. Baseline: `d3ca7a9` (accepted validation lifecycle parity). Changes are local for review; item 5 has not started.

## Observed red failures

The regression tests were run against the old implementation before their fixes. Additional compiled stream/argument cases used an isolated archive of the same baseline; the direct-cancellation check also used a temporary Go overlay restoring only the old background run context.

| Regression | Observed pre-fix behavior | Required behavior |
| --- | --- | --- |
| Empty/blank logs | Empty file emitted one blank record; trailing blank lines disappeared | Preserve actual line boundaries |
| Writer failure | Text logs returned success after output failed | Return the writer error |
| Tail/follow handoff | Callback appended a line, but output was `["two","one"]` | Emit `["two","three"]` without replay |
| Early JSON errors | Unknown target/project, invalid flags, missing arguments and missing/broken adapters had empty stdout | One typed failure result |
| Direct/attached failed tasks | Direct error was a string; attached failure discarded its nodes/log evidence | Shared error object plus available run evidence |
| Disconnected daemon | Empty stdout and plain `EOF` on stderr | Structured transport failure |
| Attached watch | Pretty start metadata, a plain banner and an event spread across 13 stdout lines; no terminal result | Three valid JSONL records: start, event, error |
| Extra positionals | Graph/log/status commands accepted extra arguments; run/watch could ignore subsequent flags | Reject malformed commands before execution |
| Adapter build cancellation | Waited about five seconds on descendant-held pipes, returned success and replaced the previous binary/key | Cancel promptly and preserve previous artifact/key |
| Localbuild lock wait | Stayed blocked after context cancellation | Release the wait with its cancellation/deadline cause |
| Finite subprocess cancellation | Waited about five seconds for a descendant to close inherited output | Stop the owned process tree |
| Repository command cancellation | Both Git output paths lost `context.Canceled` and waited about five seconds | Preserve the cause and stop descendants |
| Direct CLI cancellation | Restoring `context.Background()` made the paused task wait for its fallback, reporting `task_failed` | Propagate cancellation, stop registered handles, and skip commit/push |

The first compiled JSON matrix reported 13 failing subcases. Later tests added unknown targets/projects through direct and attached paths, invalid validation enums, action input ordering and extra-argument refusal, misleading JSON flag values, explicit false, and `--` handling.

## Additional guards

- Tail/follow covers append during a multichunk tail read, shrink/replacement with a temporarily absent pathname, short cursor-prefix changes, old partial lines at replacement, UTF-8 split across writes, blank lines, cancellation and deadlines.
- A 128 MiB sparse-file suffix and one million streamed lines exercise bounded reading; lines above 4 MiB and rewrites during a read fail explicitly.
- Compiled CLI checks assert exactly one finite document or valid compact JSONL, nonzero failure status and retained node/log/validation evidence. They reject duplicated error presentation across installed/generated entrypoints.
- Source classifications retain error causes across daemon transport and cleanup; unrelated prose containing error-code-like words is not interpreted as a code.
- A real Unix signal interrupts adapter bootstrap and yields one `operation_cancelled`/`bootstrap` result. Windows-specific tests cover child exit/result ownership, cancellation after result emission, independent daemon console groups, and preservation of daemon descendants after forced observer cancellation. Console-specific coverage skips when Windows provides no console; the forced observer case does not require one. They require native Windows CI in addition to cross-compilation.
- Direct cancellation stops a registered PID-less handle and leaves the temporary Git repository's HEAD and index intact, with no repair commit or push attempt.

## Verification

Focused tests for CLI JSON/logging, bootstrap/process/repository cancellation, shared classification and logstream race checks passed. Final full default/race suites (including examples), vet, Staticcheck v0.8.1, govulncheck v1.6.0 (no vulnerabilities), formatting/module/diff checks and version JSON passed. Linux/Windows amd64 affected test and CLI compilation passed; Windows CLI/daemon compilation was repeated after the final ownership fix.

During concurrent verification, the existing five-second upgrade-output test timed out once. It passed unchanged in isolation and in the final default and race suites. An earlier multi-file bootstrap key test ran while Go source was being edited and observed a changed build key; final suites used frozen source. These runs did not require weakened assertions or increased test timeouts.

```bash
go test ./internal/cli -run 'TestBootstrapJSON|TestCompiled.*JSON|TestLogs|TestBootstrapInterrupt|TestLocalProjectBuild.*Cancellation|TestRunCancellation' -count=1
go test -race ./internal/logstream ./internal/clierror ./internal/reporepair ./pkg/process -count=1
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
go mod tidy -diff
go run ./cmd/devflow version --json
git diff --check
```

Linux and Windows amd64 compilation covers CLI, daemon, engine, process, repository, logstream, lock and error test binaries plus `cmd/devflow`. Cross-compilation is not native execution. Real Docker/PTY workflows remain separate checks.

## Boundaries

Current-file polling cannot recover overwritten attempts or distinguish every identical-content rewrite. Cancellation of an observer/client wait does not imply cancellation of daemon-owned work. Windows bootstrap attempts bounded graceful interruption before forced cleanup; forced termination cannot establish cleanup of arbitrary external resources. Existing execution ownership/recovery remains authoritative. Persistent run/attempt evidence, public scoped cancellation, unattended prompts and operation deadlines are item 5.
