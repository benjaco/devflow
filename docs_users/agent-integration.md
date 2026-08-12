# Agent Integration

Devflow is designed so humans and agents use the same execution surface:
- operational CLI commands have stable JSON output
- instance and task state are persisted
- logs are addressable by instance and task
- one per-worktree daemon owns mutable dev/watch/operator work and publishes a typed event stream for live consumers
- `run --ci` is the exception: it stays direct and finite for CI-style validation

Agents should use the normal installed command:

```bash
go install github.com/benjaco/devflow/cmd/devflow@latest
devflow docs setup
```

Updates are intentionally Go-first:

```bash
devflow upgrade
```

Because project graph definitions are Go code, Go is expected to be available on machines where agents use Devflow.

`devflow docs setup` prints only the bundled setup/pipeline Markdown docs for the installed version. `devflow docs development` prints only the day-to-day CLI/TUI/operator docs. Both commands intentionally have no JSON mode. Use the scoped docs command that matches the task instead of fetching all docs or browsing the repository.

The intended sequencing is:
1. CLI
2. stable JSON contracts
3. typed event stream
4. TUI
5. MCP wrapper

## Readiness Workflow

For AI coding agents, `devflow flush --json` is the readiness gate when a daemon-owned watch loop is available or desired.

Recommended loop:
1. Edit files.
2. Run `devflow flush [target] --json`.
3. If `success=true`, run focused tests or other validation commands.
4. If `success=false`, inspect `issues`, `nodes`, `services`, and referenced logs before editing again.

Do not run downstream tests before a successful flush when relying on detached watch/dev mode. The flush sync sentinel proves the watcher has observed the post-edit boundary and has settled the selected target closure.

For prerequisite checks, prefer the same target scope the agent will use for execution:

```bash
devflow doctor --target up --json
devflow clis status --target up --json
```

Target-scoped required CLI checks only include `RequiredCLIs` attached to the selected target and its task closure, so agents are not blocked by tools needed only for unrelated targets.

Avoid using attached `devflow run <service-target>` as an agent readiness gate. Attached service runs keep the terminal occupied until interrupted or until a service exits. For background development, use `devflow watch <target> --detach --json` and then `devflow flush <target> --json`; both talk to the worktree daemon. `devflow run <target> --ci --json` is finite and does not use the daemon; service tasks are started through readiness and then stopped before the command returns.

For finite test/check targets that depend on services, use `devflow run <target> --ci --json` rather than plain `run`. Plain attached `run` is for keeping services alive in an operator terminal.

When an agent changes `devflow.project.go` inputs, outputs, or dependency edges for a finite target, use the dedicated hardening surface:

```bash
devflow validate build --mode all --json
```

Treat success as meaningful only when `orders.complete=true`. If the valid-order count exceeds the default bound, Devflow runs no permutations and returns an `order_limit_exceeded` issue; raise `--max-orders` explicitly if repeated execution is safe. Artifact results identify projected inputs, dependency outputs, undeclared writes, and missing outputs. Order results identify the exact permutation and failed task or the final artifact paths that differed.

Validation bypasses caches/stamps and runs tasks repeatedly in disposable worktree sandboxes. It rejects service/debug-service targets and does not isolate databases, networks, global caches, absolute paths, or unregistered background processes, so agents must choose finite targets with safe external effects.

`AGENTS.md` documents repository rules for coding agents. Future milestones can add project skills under `agents/skills/`.

For agents contributing to this repository, `docs_contributors/agent-memory.md` is shared long-term project memory. Read it before substantial work and update it when durable project context, mental models, or recurring constraints change.
