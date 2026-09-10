# Upgrade Project Confirmation Verification

`upgrade` previously had no project-update choice. The first CLI regressions
returned `invalid_arguments` for `--project` and `--project=false`; evidence is
in `/tmp/devflow-upgrade-project-red.log`. The module-update regression separately
failed before implementation because its requested pin/checksums stayed unchanged
(`/tmp/devflow-project-update-red.log`).

The permanent tests keep the project declaration, invocation and expected result
visible in each scenario:

- Confirmation accepts only a completed `y`/`yes` line; empty, other input and EOF
  decline. A channel observes the written prompt before cancellation, proving that
  an outstanding input wait can be released. Input size is bounded.
- Explicit project updates reject absent pins and active Devflow replacements
  before installation. Global-only recovery ignores a broken project module.
  JSON and nonterminal defaults do not read input.
- A real local Go module proxy and isolated install destination exercise exact
  tags and `latest`, installed-binary identity, changed project pins/checksums,
  global-only calls, failed installation, and hostile ambient build settings.
- Staged module-update tests preserve application requirements and quoted relative
  replacements. They reject newly activated Devflow replacements and cover failed
  downloads/checksums, real child cancellation, and concurrent module/checksum
  edits without overwriting those edits.
- Partial-failure JSON retains completed installation/cache evidence and reports
  an unsuccessful project update. Go install destination tests cover multiple
  `GOPATH` entries.

Focused commands:

```bash
go test ./internal/cli -run '^TestUpgrade' -count=1
go test -race ./internal/cli -run '^TestUpgrade' -count=1
go test -race ./internal/projectversion -run '^TestUpdate' -count=1
```

Module publication uses checksum-first, module-last replacement. These two files
cannot be renamed atomically as a pair; an ordinary module publication failure
restores the original checksums. Abrupt process/machine failure during that short
commit remains outside the rollback guarantee.

Final gates and the actual-terminal demo are recorded in `PROGRESS.md`. Tests
install only into temporary fixture destinations and isolate Devflow's user cache;
they do not upgrade the user's installation or operate existing services.

The actual-terminal smoke used plain `upgrade --worktree <fixture> --version
v1.2.3`, without `--project`. Both invocations displayed the prompt and installed
v1.2.3. Answering `y` changed the project's pin from v1.0.0 to v1.2.3; answering
`n` preserved its module and checksum files. Records are in
`/var/folders/g6/jbsk5f6s7ll59hy734gmngwc0000gq/T/devflow-upgrade-pty-evidence-jksbbjrl`
and `/tmp/devflow-upgrade-final-pty.json`. This local macOS terminal check does not
establish native Windows terminal behavior.

Final macOS/Go 1.27.1 gates passed: full normal tests (CLI 119.677s), full race
tests (CLI 128.934s), vet, Staticcheck v0.8.1, govulncheck v1.6.0, tidy-diff,
root/example builds, version JSON, formatting and diff checks. Linux/Windows amd64
CLI/projectversion tests and CLI binaries cross-compiled. Logs and exact quality
commands are retained under `/tmp/devflow-upgrade-final-*`.
