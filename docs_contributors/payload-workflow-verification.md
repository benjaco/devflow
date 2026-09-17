# Payload workflow verification

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

Fresh Hanji questions hide the cursor; selection redraws do not. The parser uses
that boundary and waits for output to settle before reading the menu, avoiding
duplicate questions from arrow-key redraws. It tolerates asynchronous application
logs appended to the final option. Confirmation messages preserve warning text.
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
2. Edits `collections/Posts.mjs` to rename `title` to `headline`, waits for both
   the pending question and the painted TUI selection dialog, and sends Down and
   Enter through the terminal. It verifies the answered prompt belongs to the
   new ready attempt, the renamed column retains its value, and `title` is gone.
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

## Portable coverage and limits

```sh
go test ./pkg/database -run 'PayloadPrompt|PayloadAuthoring|PayloadCMSDevService|PayloadCMSMigrationAction'
go test ./pkg/engine -run 'PromptWait|HealthDuring'
go test ./pkg/instance ./internal/cli ./pkg/tui -run 'Choice|Select|CompactPrompt'
go test ./pkg/daemon -run LifecycleReplacementPreservesExplicitPromptPolicy
go test ./pkg/process -run TerminalRetains
```

Portable helper processes test real terminal input, split ANSI/UTF-8 output,
repeated menus, exact choice encoding, decline without retries, rejected/missing
handlers and cancellation. Store/CLI cases cover choice zero, invalid types and
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
