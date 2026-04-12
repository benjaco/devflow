# Agent Integration

Devflow is designed so humans and agents use the same execution surface:
- CLI commands have stable JSON output
- instance and task state are persisted
- logs are addressable by instance and task
- the engine publishes a typed event stream for live consumers

The intended sequencing is:
1. CLI
2. stable JSON contracts
3. typed event stream
4. TUI
5. MCP wrapper

`AGENTS.md` documents repository rules for coding agents. Future milestones can add project skills under `agents/skills/`.
