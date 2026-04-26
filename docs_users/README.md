# Adopting Devflow In A Project

This guide is for teams that want to use Devflow in an application repository.

It is not about developing Devflow itself. For that, use `docs_contributors/README.md`.

## Mental Model

Devflow runs a Go-defined development graph that lives in the project repo as `devflow.project.go`.

The project owns:
- task names and commands
- target names such as `up`, `test`, or `fullstack`
- file inputs that trigger cache invalidation and watch reruns
- service readiness checks
- dependency requirements
- runtime env layering

Devflow owns:
- graph scheduling
- cache restore/snapshot mechanics
- process supervision
- detached watch mode
- flush readiness coordination
- instance state, logs, ports, and JSON surfaces

Keep project-specific behavior in `devflow.project.go`. Do not add project-specific paths or framework assumptions to Devflow core packages.

## Prerequisites

Install Go first. Devflow needs Go because project graph definitions are Go code.

```bash
go install github.com/benjaco/devflow/cmd/devflow@latest
devflow version
devflow docs
```

Make sure `$(go env GOPATH)/bin` is on `PATH`.

`devflow docs` prints the bundled user-facing Markdown docs for the installed Devflow version. It intentionally does not print contributor docs.

Update later with:

```bash
devflow upgrade
```

## Add Devflow To A Repo

1. Add `.devflow/` to `.gitignore`.
2. Add `devflow.project.go` at the project root.
3. Define one small target first, usually `up` or `test`.
4. Run `devflow graph list --json`.
5. Run `devflow run <target> --json`.
6. Add cache inputs/outputs and service readiness once the basic graph works.

Minimal `devflow.project.go`:

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

func (localProject) Name() string { return "my-app" }

func (localProject) DefaultTarget() string { return "up" }

func (localProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "my-app"}, nil
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

Replace the `check` command with a real project command once the bootstrap works.

## Design The Graph

Start with a graph that matches how developers already think about the repo.

Common task kinds:
- `project.KindOnce`: finite command such as build, codegen, lint, or test
- `project.KindWarmup`: finite prep command that may be skipped or blocked in watch mode unless explicitly allowed
- `project.KindService`: long-running process supervised by Devflow
- `project.KindGroup`: grouping node for a target shape

Common target shapes:
- `test`: checks that should finish
- `up`: local development services
- `fullstack`: all services and required prep tasks
- `codegen`: generated artifacts only

Example target:

```go
func (localProject) Targets() []project.Target {
	return []project.Target{
		{Name: "test", RootTasks: []string{"unit_test"}},
		{Name: "up", RootTasks: []string{"api_dev", "web_dev"}},
	}
}
```

## Cacheable Tasks

Only mark finite tasks cacheable. Service tasks are supervised, not cached.

A cacheable task must declare outputs:

```go
{
	Name:  "codegen",
	Kind:  project.KindOnce,
	Cache: true,
	Inputs: project.Inputs{
		Files: []string{"schema.json"},
	},
	Outputs: project.Outputs{
		Dirs: []string{"internal/generated"},
	},
	Run: func(ctx context.Context, rt *project.Runtime) error {
		return rt.RunCmd(ctx, "go", "run", "./tools/codegen")
	},
}
```

Prefer narrow semantic inputs over hashing the whole repository. Add files, dirs, env vars, and custom fingerprints that actually affect the task result.

Task cache storage is global for the user:

```text
<os.UserCacheDir()>/devflow/cache/
```

Entries are namespaced by project. By default the namespace is `Project.Name()`. Override it only when you need a more stable or collision-resistant namespace:

```go
func (localProject) CacheNamespace() string { return "company-my-app" }
```

## Services And Readiness

Service tasks start long-running processes:

```go
{
	Name: "api_dev",
	Kind: project.KindService,
	Deps: []string{"codegen"},
	Inputs: project.Inputs{
		Dirs: []string{"cmd/api", "internal/generated"},
	},
	Run: func(ctx context.Context, rt *project.Runtime) error {
		_, err := rt.StartService(ctx, "go", "run", "./cmd/api")
		return err
	},
	Ready: project.ReadyHTTPNamedPort("api", "/health", 200),
}
```

Use readiness hooks for services that need a health check before downstream work or `flush` can report success.
For named-port readiness, include that port name in `InstanceConfig.PortNames` and pass the assigned port to the service through env or args.

## Watch Mode

Watch mode maps changed files to task inputs, then reruns the affected downstream slice.

Run:

```bash
devflow watch up --detach --json
```

Then after edits:

```bash
devflow flush up --json
```

`flush` proves the watcher has processed a sync sentinel written after your edits. It returns success only when the selected target closure has settled and in-chain services are healthy.

Important watch rules:
- downstream jobs do not run past blocked intermediate tasks
- services default to affected-slice restarts
- `RestartNever` prevents watch restarts
- `RestartAlways` restarts a service on any watch cycle that affects the target

## Runtime State

Per-worktree state stays under the project worktree:

```text
.devflow/state/
.devflow/logs/
.devflow/bin/
.devflow/localbuild/
```

Do not commit `.devflow/`.

Sibling worktrees can have isolated instance state and logs while sharing the global task cache.

## Environment

Adapters can load `.env` files and then layer Devflow-managed values on top.

Recommended precedence:
1. `.env`
2. adapter defaults
3. Devflow-managed runtime values such as ports and DB URLs

Use `project.LoadOptionalDotEnvInWorktree` and `project.MergeEnvMaps` for this instead of hand-rolling env parsing.

## Dependencies

Expose required commands through `Dependencies()`:

```go
func (localProject) Dependencies() []project.Dependency {
	return []project.Dependency{
		{Name: "go", Command: "go"},
		{Name: "npm", Command: "npm"},
	}
}
```

Then users can run:

```bash
devflow deps status --json
devflow doctor --json
```

Install scripts are supported for dependencies that can be installed safely and idempotently.

## Daily Workflow

Typical human workflow:

```bash
devflow
devflow status --json
devflow logs api_dev
devflow stop --all --json
```

Typical agent workflow:

```bash
devflow watch up --detach --json
# edit files
devflow flush up --json
# run focused tests only after success=true
```

If `flush` fails, inspect `issues`, `nodes`, `services`, and referenced log paths before retrying.

## What To Commit

Commit:
- `devflow.project.go`
- project files referenced by task inputs
- generated source only if your project normally commits it
- docs explaining your project targets

Do not commit:
- `.devflow/`
- local logs
- worktree-local binaries
- generated build modules under `.devflow/localbuild`

## More Detail

Use these next:
- `docs_users/adapter-guide.md` for adapter APIs and deeper patterns
- `docs_contributors/cli.md` for command and JSON behavior
- `docs_users/agent-integration.md` for AI workflow expectations
- `docs_contributors/architecture.md` for runtime state and package boundaries
