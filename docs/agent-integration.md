# Agent Integration

Devflow is designed so humans and agents use the same execution surface:
- CLI commands have stable JSON output
- instance and task state are persisted
- logs are addressable by instance and task

The intended sequencing is:
1. CLI
2. stable JSON contracts
3. TUI
4. MCP wrapper

`AGENTS.md` documents repository rules for coding agents. Future milestones can add project skills under `agents/skills/`.
