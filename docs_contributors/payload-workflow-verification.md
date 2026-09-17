# Payload workflow verification

Native Windows validation now includes the real engine workflow and an expanded
compiled-CLI TUI workflow with migration authoring. See the
[Windows audit](windows-verification.md) for failures, corrections, opt-ins and
platform limits. Earlier macOS/CI-only statements below describe their original
verification runs.

The reported version is Payload 3.88.0. The reproduction installs unmodified
`payload` and `@payloadcms/db-postgres` 3.88.0 with Next 15.4.11 and React 19.1.2
from a committed npm lockfile. It runs against disposable Postgres databases
through Devflow's real engine, watch loop, process terminal and persisted prompt
store. The local run used macOS arm64, Node 26.7.0 and Docker Desktop. The native
Linux amd64/arm64 Docker CI job runs the same test with Node 24.

## Failure and correction

Payload's Drizzle/Hanji conflicts are arrow-key menus, including separate UP and
DOWN migration questions. Literal yes/no patterns cannot represent their choices.
Development schema push can emit the same menus from inside Next, where the
previous helper installed no interaction handling. Both paths now use an owned
terminal and translate rendered questions into generic Devflow select/confirm
prompts. Payload recognition and input encoding remain in the database adapter.

The parser waits for output to settle and recognizes the latest question text.
ConPTY can coalesce cursor visibility between questions, position rows with cursor
controls and compress spaces into forward movements, so a fresh cursor-hide
sequence is not required. Completed output and redraws with a later option
selected do not reopen questions: a fresh Hanji menu starts on create and the
adapter sends only Down/Return. It tolerates asynchronous application logs
appended to the final option. Confirmation messages preserve warning text.
The `prompts` library accepts `y` immediately, so sending another Return would
risk accepting a following question. A decline stops the process as a failure:
Payload can otherwise exit zero without writing a migration and trigger the
new-file convergence retry.

The dashboard displays choices with Up/Down and Enter; Escape cancels without
quitting the dashboard. CLI/agent clients inspect `prompts list --json`, then use
`prompts respond ... --choice <zero-based-index> --json` or `--cancel`. Answers
retain the existing exact run/task/attempt identity and liveness checks. Headless
failure stops promptly with `interaction_required`; explicit cancellation uses
`interaction_cancelled`. Startup readiness excludes the current attempt's operator
wait, while operation and prompt deadlines remain active. A waiting service must
not commit readiness or report healthy through flush.

Development push and migration replay use separate databases, following
[Payload's migration workflow](https://payloadcms.com/docs/database/migrations).
The example's `up` target no longer applies migrations into its pushed development
database; `migrate` requires a separate `PAYLOAD_MIGRATION_DATABASE_URL`. Authoring
uses snapshots without starting Postgres. Both migration commands honor the
configured `PAYLOAD_CONFIG_PATH`. Default watched schema inputs now include
`src/blocks`; additional plugin/config locations still need explicit inputs.
Migration action metadata identifies configured development services and only
declared migration tasks, so removing migration replay from development does not
break the dashboard's selected-task migration shortcut.

## Real workflow regression

```sh
DEVFLOW_E2E_PAYLOAD=1 go test -count=1 -timeout=15m ./examples/payloadcms-postgres -run TestPayloadWorkflowE2E -v
```

The committed fixture and test cover:

1. Boot real Next/Payload, push a collection and block, and insert existing data.
2. Author the initial migration, replay it into a separate database and seed that
   database too.
3. Start the actual watch loop, rename a collection field, block table and block
   field, answer the development menus and verify a new ready service attempt.
   Assert all seeded values survive.
4. Author both migration directions, answering six rename questions. Inspect the
   SQL, replay the UP migration and verify existing data survives there too.
5. Restart unchanged with no prompt. Choose create rather than rename on a later
   field change and assert ADD/DROP SQL.
6. Decline a blank migration exactly once without changing history; accept the
   next blank migration and require a new file.
7. Exercise headless failure and explicit cancellation, retaining the question
   and its structured failure code.
8. Attempt migration replay on the pushed database, surface Payload's warning,
   decline and verify the development data remains.
9. Decline a destructive development push, verify the column still exists, then
   retry and explicitly accept. Verify unaffected field/block values remain.

This workflow passed locally, including the watch variant. The tests clean up
their own engines, services, databases, container and volume. They do not use a
developer's project or development database.

## Compiled CLI and terminal TUI workflow

```sh
DEVFLOW_E2E_PAYLOAD=1 go test -count=1 -timeout=12m ./examples/payloadcms-postgres -run '^TestPayloadTUIWorkflowE2E$' -v
```

This separate end-to-end test starts with a fresh Next/Payload fixture and no
`node_modules`, using the same pinned npm lockfile. It builds the actual Devflow
CLI and launches bare `devflow` in a real PTY with a controlling terminal. The
normal CLI bootstraps `devflow.project.go`, starts its daemon/watch loop, runs
`npm ci` and starts Next/Payload. No dashboard, daemon or prompt callbacks are
substituted. Compiler/download caches can be shared; runtime state is isolated.

The test then:

1. Waits for initial Payload database initialization and watch readiness, then
   inserts a row into the real `posts` table.
   Uses `m` to name and create the initial migration, requires the generated
   table SQL, and waits for the previous watch target to resume.
2. Edits `collections/Posts.mjs` to rename `title` to `headline`, waits for both
   the pending question and the painted TUI selection dialog, and sends Down and
   Enter through the terminal. It verifies the answered prompt belongs to the
   new ready attempt, the renamed column retains its value, and `title` is gone.
   Uses `m` again to author the rename, answers both UP and DOWN selection
   questions through the terminal, checks both SQL directions and their shared
   run/attempt identity, then verifies watch resumes and development data survives.
3. Removes populated `legacy` from that collection module, waits for the painted
   warning and Yes/No buttons, and sends Left and Enter to move from the default
   No to Yes. It verifies `legacy` still exists before the answer, disappears
   afterward, and the other value survives.
4. Sends `q` through the TUI and requires exit zero, daemon exit and termination
   of every observed Next service PID before removing the test database/volume.

All answers use terminal input. Prompt files are read only for assertions; the
test never calls the prompt response API. Assertions inspect emitted TUI labels
after the edit and normalize terminal painting controls; they do not claim pixel
or terminal-host visual fidelity. Failure cleanup uses the public `stop --all`
command and joins the terminal reader before removing temporary files.

The full terminal workflow passed locally on macOS arm64, both normally and with
`go test -race`; the child CLI uses its ordinary bootstrap build. The combined
engine and TUI test invocation also passes. Evidence is retained in
`/tmp/devflow-payload-tui-final.log` and `/tmp/devflow-payload-tui-race.log`.
The Docker CI gate now
selects both tests with `-run 'TestPayload(TUI)?WorkflowE2E' -timeout 25m` on native
Linux amd64 and arm64. Hosted outcomes and native Windows execution remain CI or
platform-specific evidence, not inferred from a local pass.

The expanded authoring workflow passes natively on Windows amd64. It reproduced
two additional issues: CRLF checkout line endings prevented the deleted-field
fixture edit, and authoring discarded the previous watcher's headless wait policy.
The declaration edit is now line-ending independent. Action relaunch preserves
the previous policy without inheriting the completed action's deadline or
cancellation. Portable daemon tests separately cover preservation of both wait
and fail policies. The terminal test never submits answers through prompt files
or APIs.

## Portable coverage and limits

```sh
go test ./pkg/database -run 'PayloadPrompt|PayloadAuthoring|PayloadCMSDevService|PayloadCMSMigrationAction'
go test ./pkg/engine -run 'PromptWait|HealthDuring'
go test ./pkg/instance ./internal/cli ./pkg/tui -run 'Choice|Select|CompactPrompt'
go test ./pkg/daemon -run LifecycleReplacementPreservesExplicitPromptPolicy
go test ./pkg/process -run TerminalRetains
```

Portable helper processes test real terminal input, split ANSI/UTF-8 output,
repeated menus with and without fresh cursor controls, exact choice encoding,
decline without retries, rejected/missing handlers and cancellation. Rendering
regressions cover positioned rows, compressed spacing and selection redraws.
Store/CLI cases cover choice zero, invalid types and
indexes, stale/duplicate responses, reconnect and value-free acknowledgments.
Compact views preserve the entire choices list or omit it with a truncation
indication. Dashboard tests drive a real tview event loop with Down/Enter/Escape.
Virtual-time readiness tests cover long operator waits, unrelated task prompts,
operation deadlines and deferred readiness commits. Daemon lifecycle replacement
tests retain selection metadata and explicit wait policy.

This is structured support for the observed Payload 3 prompt protocols, not a
general terminal emulator. Native ConPTY execution and hosted Linux workflow
results require CI; Windows cross-compilation alone cannot validate rendering.
The real workflow verifies PostgreSQL UP replay and both generated SQL directions;
it does not execute rollback, other database adapters, or the user's own project.

## Local validation

The normal and race suites pass with Delve absent from the test PATH, using the
existing real-debugger test skip. With the original PATH, two real Delve tests
time out: `TestExampleProjectBackendDebugTarget` and
`TestGoDebugServiceWatchRestartsRealDelveAfterSourceEdit`. `DevToolsSecurity
-status` reports disabled on this host; no machine security settings were changed.
See the macOS prerequisites in [testing](testing.md). This leaves real Delve
execution unverified locally, rather than treating the reduced run as that proof.

`go vet ./...`, Staticcheck v0.8.1, govulncheck v1.6.0 (no vulnerabilities),
`go mod tidy -diff`, CLI/example builds, version JSON, formatting and diff checks
pass. Affected package tests cross-compile for Windows amd64. The final action
metadata and builder-order correction also has focused normal/race coverage.

Session evidence is retained in `/tmp/devflow-payload-e2e-watch.log`,
`/tmp/devflow-payload-full.log`, `/tmp/devflow-payload-full-without-delve.log`,
`/tmp/devflow-payload-race.log` and `/tmp/devflow-payload-quality/`.

## PR #26 Windows follow-up

The [Windows job at the PR head](https://github.com/benjaco/devflow/actions/runs/35213350923/job/105175918452)
failed the deleted-field migration confirmation and the terminal rename/create/
decline cases with context deadlines. Headless, cancel and missing-handler cases
passed, as did both native Linux Docker workflows. The Windows job did not retain
the child terminal bytes; its diagnostics alone cannot identify the exact render.

The unchanged failing tests pass on macOS. Portable synthetic terminal frames
then reproduce two unsupported ConPTY behaviors before correction: confirmation
matching requires a literal trailing space, and subsequent menus/confirmations
require a new cursor-hide sequence. The latter is not preserved by
[ConPTY's frame renderer](https://github.com/microsoft/terminal/blob/a8582978afa50ece88edc7eda9e182ced64876e2/src/renderer/vt/XtermEngine.cpp#L95).
The synthetic frames are not claimed as captures from this Windows job.

The correction stays in the Payload adapter. Literal patterns end at the colon;
structured parsing locates the latest question and handles positioned rows and
forward spacing, while rejecting completed/selected redraws. Real-terminal
helpers exercise two consecutive menus followed by a warning, exact answers,
decline, headless failure and cancellation under both renderings. Failures now
report the prompt sequence and escaped child output for native diagnosis.

```sh
go test -race -count=3 ./pkg/database -run 'TestPayloadPrompt|TestPayloadLiteralConfirmation|TestPayloadAuthoring'
```

Local reproduction and focused validation logs: `/tmp/devflow-pr26-baseline.log`,
`/tmp/devflow-pr26-red.log`, `/tmp/devflow-pr26-green.log` and
`/tmp/devflow-pr26-focused-race.log`. Native Windows verification still requires
the corrected branch's CI run.

Both real Payload workflows pass together (`/tmp/devflow-pr26-payload-e2e.log`),
along with the full normal suite, vet, Staticcheck, govulncheck, tidy-diff,
CLI/example builds, version JSON and affected Windows test cross-compilation.
Local suites use the existing Delve skip because Developer Tools security is
disabled, as described above. The initial parallel race suite hit the unchanged
500 ms daemon deadline fixture (`TestRunRequestDeadlineCancelsExecution/attached`);
three isolated race repetitions and the full `go test -race -p 1 -count=1 ./...`
recheck pass without changing that fixture. Logs: `/tmp/devflow-pr26-full.log`,
`/tmp/devflow-pr26-race.log`, `/tmp/devflow-pr26-daemon-recheck.log` and
`/tmp/devflow-pr26-race-serial.log`.

### Captured terminal-title follow-up

The [next Windows job at `05c4b88`](https://github.com/benjaco/devflow/actions/runs/35228380375/job/105225702804)
passed the earlier failures but timed out in the cancel scenario before publishing
any prompt. Its retained bytes show a terminal-title OSC inserted between `re`
and `named` in the question. Removing CSI controls alone leaves that word broken.

Replaying the captured bytes locally reproduces zero callbacks and the ten-second
deadline before correction. The adapter now removes complete OSC strings before
interpreting cursor controls, without adding whitespace between the surrounding
characters. It waits for incomplete strings and supports both
[documented title terminators](https://learn.microsoft.com/en-us/windows/console/console-virtual-terminal-sequences#window-title),
BEL and ST. Metadata cannot become a structured question or change its visibility.

`TestPayloadPromptConPTYTitle` checks the captured menu, exact choices/keystrokes,
titles inside choice and confirmation text, and incomplete strings.
`TestPayloadConPTYTitleCancellation` replays the bytes through the interactive
process reader using pipes to preserve them on every host, then requires one
callback and cancellation rather than deadline expiry. Existing PTY/ConPTY
authoring scenarios remain in the same focused race run.

Red/green evidence: `/tmp/devflow-pr26-osc-red.log` and
`/tmp/devflow-pr26-osc-focused.log`. Native Windows execution remains a separate
CI verification; captured-byte replay does not substitute for it.

Three focused race repetitions pass, with the captured cancellation completing
in 0.12 seconds each time. The full normal suite, vet, Staticcheck, builds,
tidy-diff, version JSON and Windows database test cross-compilation pass. The
normal suite retains the existing local Delve skip. Both real Payload E2Es were
attempted but failed before project startup because the local Docker socket was
absent; launching Docker Desktop did not restore daemon access. This rerun does
not establish real workflow success for the OSC correction. Logs:
`/tmp/devflow-pr26-osc-full.log` and `/tmp/devflow-pr26-osc-e2e.log`.
