# Devflow

Devflow is a local-first DAG runner for development workflows.

It gives a project a small Go-defined task graph with:
- cached one-shot tasks
- supervised long-running services
- service readiness checks
- detached watch/dev supervisors
- file-change cascades through the task graph
- sandboxed input/output and exhaustive valid-order pipeline validation
- `devflow flush --json` as an AI readiness gate
- stable JSON output for humans, CI, and coding agents

Devflow stays generic. Project-specific behavior belongs in the project-owned Go adapter sources or in example adapters, not in the core packages.

## Documentation

There are two documentation lanes:

- **Use Devflow in your project**: start with this README, then use `devflow docs setup` for pipeline setup or `devflow docs development` for daily usage.
- **Develop Devflow itself**: start with `docs_contributors/README.md`, then read `AGENTS.md`, `docs_contributors/agent-memory.md`, and `PROGRESS.md`.

Keep these separate when adding docs. Project adopters should not need contributor internals before they can define a useful `devflow.project.go`.

## Install

Devflow requires Go 1.26.6 or newer because project graph definitions are Go code.

```bash
go install github.com/benjaco/devflow/cmd/devflow@latest
devflow version
devflow docs setup
```

Make sure `$(go env GOPATH)/bin` is on your `PATH`; that is where `go install` places the `devflow` executable by default. If `devflow upgrade` succeeds but `devflow version` does not change, run `which -a devflow`: another command earlier on `PATH` is shadowing the Go-installed binary.

Update later with:

```bash
devflow upgrade
```

`devflow upgrade` is intentionally simple in round 1. It runs:

```bash
go install github.com/benjaco/devflow/cmd/devflow@latest
```

There are no release binaries, npm package, Homebrew tap, or installer scripts yet.

## Getting Started

This is the short path for adding Devflow to another project. The longer setup guide is available through `devflow docs setup`.

In the project you want Devflow to run, add `devflow.project.go`:

```go
package main

import (
	"context"

	"github.com/benjaco/devflow/pkg/project"
)

func init() {
	project.Register(project.Define(func(ctx context.Context, b *project.Builder) error {
		b.Name("my-project")
		b.DefaultTarget("up")
		b.RequiredCLIs("go")

		check := b.Task("check").Command("go", "version")
		b.Target("up", check)
		return nil
	}))
}
```

Replace the `check` task command with the project command you actually want, such as `go test ./...`, `npm test`, or a service start command.

Small adapters can remain in that one file. Larger adapters may opt into root-level `devflow_*.go` companions, all using `package main`:

```text
devflow.project.go       # small required entrypoint and project registration
devflow_shared.go        # shared constants, environment, helpers
devflow_frontend.go      # frontend tasks and services
devflow_backend.go       # backend and database tasks
devflow_ci.go            # CI, deployment, and E2E targets
devflow_watch_test.go    # normal Go test; excluded from runtime bootstrap
```

Then run:

```bash
devflow graph list --json
devflow run up --json
```

For a detached watch workflow:

```bash
devflow watch up --detach --json
devflow flush up --json
```

`flush` writes a sync sentinel, waits for the watcher to process file changes before that sentinel, waits for the selected target closure to settle, and reports structured success or issues. Coding agents should edit files, run `devflow flush --json`, and only run tests after `success=true`.

Bare `devflow` inside a project worktree starts the default target detached when needed and opens the TUI.
The TUI keeps graph order stable, distinguishes service startup/readiness/restart/failure states, follows running logs until you scroll up, and uses `?` for contextual help. Lifecycle actions show their stop/execute/preserve scope before execution.

## Project Model

Current project-local constraints:

- the project repo owns `./devflow.project.go`
- `devflow.project.go` remains the mandatory marker and normally registers the project in `init()`
- root-level `devflow_*.go` files are optional companions; every adapter source must use `package main`
- `devflow_*_test.go`, unrelated sibling Go files, nested directories, and symlinks are not loaded into the runtime adapter
- importing `github.com/benjaco/devflow/pkg/...` and standard library packages is supported

When Devflow sees `devflow.project.go`, it compiles a worktree-local CLI into:

```text
<worktree>/.devflow/bin/devflow-local
```

Generated build modules live under:

```text
<worktree>/.devflow/localbuild/<hash>/
```

Commit `devflow.project.go` and any `devflow_*.go` companions. Keep ordinary `devflow_*_test.go` adapter tests in the repo as usual. Do not commit `.devflow/`.

## Common Commands

```bash
devflow docs setup
devflow docs development
devflow version --json
devflow doctor --json
devflow graph list --json
devflow graph show up --json
devflow validate build --mode all --json
devflow run up --json
devflow watch up --detach --json
devflow flush up --json
devflow status --json
devflow logs <task>
devflow restart <service> --preview --json
devflow restart <service> --json
devflow stop --task <service> --preview --json
devflow stop --task <service> --json
devflow stop --all --json
devflow cache status --json
devflow cache path --json
devflow cache key --target build --json
```

All user-facing commands are expected to keep stable JSON output except `devflow docs setup` and `devflow docs development`, which intentionally print scoped plain user Markdown.

## State And Cache

Per-worktree runtime state lives under the project worktree:

```text
<worktree>/.devflow/state/
<worktree>/.devflow/logs/
```

Task cache storage is shared system-wide under the OS user cache directory:

```text
<os.UserCacheDir()>/devflow/cache/
```

Cache entries are namespaced by project, so sibling worktrees and unrelated project worktrees can share one physical cache folder without sharing instance state.

## Examples

The repo includes example adapters that double as smoke coverage:
- `examples/go-next-monorepo`
- `examples/web-worker-workspace`
- `examples/embedded-web-app`

They show larger graphs with services, generated artifacts, watch reruns, required CLI checks, and database helpers.

## Developing Devflow

This section is only for contributors changing this repository. For full contributor guidance, read `docs_contributors/README.md`.

For work on Devflow itself:

```bash
go test ./...
go build -o .devflow/bin/devflow ./cmd/devflow
```

You can also use the repo-local launcher:

```bash
./devflow version
```

Start substantial agent or contributor work by reading:
- `AGENTS.md`
- `docs_contributors/agent-memory.md`
- `PROGRESS.md`

More docs:
- `docs_users/README.md`
- `docs_users/setup.md`
- `docs_users/development.md`
- `docs_contributors/README.md`
- `docs_contributors/architecture.md`
- `docs_contributors/cli.md`
- `docs_users/adapter-guide.md`
- `docs_users/agent-integration.md`
- `docs_contributors/testing.md`
- `docs_contributors/roadmap.md`
