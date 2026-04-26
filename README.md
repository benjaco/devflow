# Devflow

Devflow is a local-first DAG runner for development workflows.

It gives a project a small Go-defined task graph with:
- cached one-shot tasks
- supervised long-running services
- service readiness checks
- detached watch/dev supervisors
- file-change cascades through the task graph
- `devflow flush --json` as an AI readiness gate
- stable JSON output for humans, CI, and coding agents

Devflow stays generic. Project-specific behavior belongs in the project-owned `devflow.project.go` file or in example adapters, not in the core packages.

## Documentation

There are two documentation lanes:

- **Use Devflow in your project**: start with this README, then read `docs_users/README.md` and `docs_users/adapter-guide.md`.
- **Develop Devflow itself**: start with `docs_contributors/README.md`, then read `AGENTS.md`, `docs_contributors/agent-memory.md`, and `PROGRESS.md`.

Keep these separate when adding docs. Project adopters should not need contributor internals before they can define a useful `devflow.project.go`.

## Install

Devflow requires Go because project graph definitions are Go code.

```bash
go install github.com/benjaco/devflow/cmd/devflow@latest
devflow version
devflow docs
```

Make sure `$(go env GOPATH)/bin` is on your `PATH`; that is where `go install` places the `devflow` executable by default.

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

This is the short path for adding Devflow to another project. The longer guide is `docs_users/README.md`.

In the project you want Devflow to run, add a self-contained `devflow.project.go` file:

```go
package main

import (
	"context"

	"github.com/benjaco/devflow/pkg/project"
)

type localProject struct{}

func init() {
	project.Register(localProject{})
}

func (localProject) Name() string { return "my-project" }

func (localProject) DefaultTarget() string { return "up" }

func (localProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "my-project"}, nil
}

func (localProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name: "check",
			Kind: project.KindOnce,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				return rt.RunCmd(ctx, "go", "version")
			},
		},
	}
}

func (localProject) Targets() []project.Target {
	return []project.Target{
		{Name: "up", RootTasks: []string{"check"}},
	}
}
```

Replace the `check` task command with the project command you actually want, such as `go test ./...`, `npm test`, or a service start command.

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

## Project Model

Current project-local constraints:
- the project repo owns `./devflow.project.go`
- the file must use `package main`
- the file must register a project in `init()`
- the file must be self-contained; arbitrary companion Go files are not loaded yet
- importing `github.com/benjaco/devflow/pkg/...` and standard library packages is supported

When Devflow sees `devflow.project.go`, it compiles a worktree-local CLI into:

```text
<worktree>/.devflow/bin/devflow-local
```

Generated build modules live under:

```text
<worktree>/.devflow/localbuild/<hash>/
```

Commit `devflow.project.go`. Do not commit `.devflow/`.

## Common Commands

```bash
devflow docs
devflow version --json
devflow doctor --json
devflow graph list --json
devflow graph show up --json
devflow run up --json
devflow watch up --detach --json
devflow flush up --json
devflow status --json
devflow logs <task>
devflow stop --all --json
devflow cache status --json
```

All user-facing commands are expected to keep stable JSON output except `devflow docs`, which intentionally prints plain user Markdown.

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

They show larger graphs with services, generated artifacts, watch reruns, dependency checks, and database helpers.

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
- `docs_contributors/README.md`
- `docs_contributors/architecture.md`
- `docs_contributors/cli.md`
- `docs_users/adapter-guide.md`
- `docs_users/agent-integration.md`
- `docs_contributors/testing.md`
- `docs_contributors/roadmap.md`
