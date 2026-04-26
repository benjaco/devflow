# Agent Integration

Devflow is designed so humans and agents use the same execution surface:
- operational CLI commands have stable JSON output
- instance and task state are persisted
- logs are addressable by instance and task
- the engine publishes a typed event stream for live consumers

Agents should use the normal installed command:

```bash
go install github.com/benjaco/devflow/cmd/devflow@latest
devflow docs
```

Updates are intentionally Go-first:

```bash
devflow upgrade
```

Because project graph definitions are Go code, Go is expected to be available on machines where agents use Devflow.

`devflow docs` prints the bundled user-facing Markdown docs for the installed version. It has no JSON mode. Use it when integrating Devflow into another project instead of fetching contributor docs or browsing the repository.

The intended sequencing is:
1. CLI
2. stable JSON contracts
3. typed event stream
4. TUI
5. MCP wrapper

## Readiness Workflow

For AI coding agents, `devflow flush --json` is the readiness gate when a detached watch supervisor is available or desired.

Recommended loop:
1. Edit files.
2. Run `devflow flush [target] --json`.
3. If `success=true`, run focused tests or other validation commands.
4. If `success=false`, inspect `issues`, `nodes`, `services`, and referenced logs before editing again.

Do not run downstream tests before a successful flush when relying on detached watch/dev mode. The flush sync sentinel proves the watcher has observed the post-edit boundary and has settled the selected target closure.

`AGENTS.md` documents repository rules for coding agents. Future milestones can add project skills under `agents/skills/`.

For agents contributing to this repository, `docs_contributors/agent-memory.md` is shared long-term project memory. Read it before substantial work and update it when durable project context, mental models, or recurring constraints change.
