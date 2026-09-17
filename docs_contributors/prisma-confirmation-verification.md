# Prisma confirmation verification

The reported migration action reached Prisma, printed a data-loss warning, then
failed because stdin was not a terminal. This occurs before Prisma asks for a
response, so adding a pipe prompt handler alone cannot fix it.

The portable regression builds a Go helper as a worktree-local `npx` and runs it through
`GeneratePrismaMigrationForRuntime`. It tests a real stdin descriptor, emits a
warning and requires an explicit answer. Before the fix it exited 1 with the
non-interactive diagnostic and zero prompt callbacks. It now covers acceptance,
decline (exit 130), handler rejection, cancellation and a missing handler.
It covers both the default command and a custom executable; the default-command
case also caught the runtime callback being discarded during command construction.

```sh
go test -count=1 -v ./pkg/database -run 'TestPrismaMigrationConfirmationRequiresTerminal|TestPrismaCommandBuilders'
go test -race -count=1 ./pkg/process -run '^TestTerminal'
go test -race -count=1 ./pkg/tui -run '^TestMigrationShortcut'
```

The same tests run in the existing native Windows matrix. Cross-compilation is
only a build check; it does not verify ConPTY execution or terminal-host behavior.
Process regressions verify 2,000 retained lines, stderr content in the merged
stream, final partial output, nonzero status, secret echo suppression and
cancellation of a descendant that otherwise retains the terminal for a minute.

A separate local smoke used unmodified Prisma 6.19.0 and a disposable SQLite
database, without application dependencies or existing development services:

1. Create/apply an initial migration containing a nullable column and insert one
   row with a non-null value in that column.
2. Remove the column from the schema and run `prisma migrate dev --create-only
   --name remove-legacy --skip-generate` with piped stdin. It exits 1 after the
   data-loss warning with the same non-interactive diagnostic.
3. Run the same command through `GeneratePrismaMigrationForRuntime`, first
   answering `n`, then `y` through its runtime prompt callback.
4. Verify `n` requests confirmation once, retains Prisma exit 130 and creates no
   migration; `y` requests confirmation once, exits 0 and creates migration SQL.
   The original row/column remain intact after both create-only runs.

Both terminal outcomes passed on macOS, including a custom command with
`pnpm --dir database exec prisma migrate dev --name remove-legacy --create-only
--schema schema.prisma --skip-generate`. A separate run through the default `npx`
command also requested confirmation once and created its migration successfully.
These checks establish the terminal/prompt path; they do not verify native
Windows execution or any application's database preparation policy.
No force/auto-accept flags or Prisma implementation patches are involved.

## Windows prompt teardown follow-up

[Native Windows testing at `7d05173`](https://github.com/benjaco/devflow/actions/runs/35016655194/job/104541700144) exposed two cases that passed on macOS:
headless/canceled prompts were invoked twice when ConPTY emitted teardown output,
and the secret fixture waited for a literal space where ConPTY emitted `ESC[1C`.
The missing suppression marker meant the prompt never matched; the captured log
did not contain the secret. Its timeout also exposed interactive cancellation
being normalized into success after process cleanup.

Portable regressions reproduced all three locally before correction: three
callbacks after failure, zero secret callbacks for the captured rendering, and
nil errors/exit zero for canceled pipe and terminal commands. The reader now
disables callbacks after failure while retaining final output, cancellation is
returned independently of stop normalization, and the fixture matches `Secret:`.
Literal matching remains unchanged; no terminal parser or dependency was added.

```sh
go test -race -count=3 ./pkg/process ./pkg/database -run 'TestInteractiveReaderDoesNotPromptAgainAfterFailure|TestTerminalSecretPromptMatchesConPTYOutput|TestRunInteractiveCancellationReportsError|TestTerminalRetainsOutputAndExitStatus|TestPrismaMigrationConfirmationRequiresTerminal'
```

The focused tests originally passed on macOS, with captured-output replay checking
the matching and callback boundary. These fixtures now also pass through native
Windows ConPTY; see the [Windows audit](windows-verification.md). That fixture
coverage is separate from the real Prisma CLI smoke above and manual terminal-host
behavior.
