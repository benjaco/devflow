# Developing Devflow

This guide is for contributors changing Devflow itself.

If you are adding Devflow to an application repository, use `devflow docs setup` or `docs_users/setup.md` instead.

## Start Here

Read these first:
1. `AGENTS.md`
2. `docs_contributors/agent-memory.md`
3. `PROGRESS.md`
4. `docs_contributors/architecture.md`
5. `docs_contributors/cli.md`
6. `docs_contributors/testing.md`

`PROGRESS.md` is the implementation ledger. Update it at the start and end of substantial work.

`docs_contributors/agent-memory.md` is shared long-term AI project memory. Update it only for durable mental models, invariants, repeated failure modes, and cross-cutting context.

## Local Setup

Devflow is a Go 1.25 project. Use the latest supported Go 1.25 patch release or newer.

```bash
go test ./...
go build -o .devflow/bin/devflow ./cmd/devflow
```

The repo also has a source-local launcher:

```bash
./devflow version
```

The launcher rebuilds `.devflow/bin/devflow` when the source content build key changes, sets the source-root bootstrap env, and then runs the built binary.

Do not use plain `go build ./cmd/devflow` in the repo root. The default output name is `./devflow`, which would overwrite the tracked launcher script. Always pass `-o`.

## Source Tree

Core packages:
- `pkg/project`: public project/task/target/runtime API
- `pkg/graph`: graph validation, closures, topo order, affected-task logic
- `pkg/fingerprint`: deterministic task key inputs
- `pkg/cache`: global task cache store and snapshot/restore mechanics
- `pkg/process`: subprocess and service process handling
- `pkg/instance`: per-worktree instance state, logs, flush files, global index
- `pkg/ports`: shared port allocation
- `pkg/engine`: scheduling, run/watch execution, service readiness, flush health
- `pkg/watch`: polling file watcher
- `pkg/database`: optional Docker/Postgres helpers
- `pkg/tui`: terminal operator UI

CLI and bootstrap:
- `cmd/devflow`: thin main package
- `internal/cli`: command implementation and project-local bootstrap
- `internal/version`: build/version metadata and Go install package constants

Examples:
- `examples/go-next-monorepo`
- `examples/web-worker-workspace`
- `examples/embedded-web-app`

## Design Boundaries

Keep Devflow core generic.

Do not hardcode:
- Prisma-specific behavior
- sqlc-specific behavior
- Next.js-specific behavior
- repository-specific paths
- a YAML-first DSL

Project behavior belongs in:
- project-local `devflow.project.go`
- example adapters
- reusable generic helpers only when the abstraction is clearly framework-neutral

Every user-facing command must keep stable JSON output.

Services are supervised, not cached. Cacheable tasks must declare outputs. Stamped finite tasks record local completion by input key without storing outputs in the shared cache; use them for install/setup state such as `node_modules`.

## Bootstrap Model

Installed Devflow and the source-local launcher both compile project-local graph definitions.

When a project worktree contains `devflow.project.go`, Devflow generates:

```text
<worktree>/.devflow/localbuild/<hash>/
<worktree>/.devflow/bin/devflow-local
```

The generated module path is under:

```text
github.com/benjaco/devflow/localbuild/<hash>
```

That keeps imports of `github.com/benjaco/devflow/internal/cli` legal.

Source-local development adds:

```go
replace github.com/benjaco/devflow => <devflow-source-root>
```

Installed mode requires the released Devflow module version instead.

## State And Cache

Per-worktree runtime state:

```text
<worktree>/.devflow/state/
<worktree>/.devflow/logs/
<worktree>/.devflow/bin/
<worktree>/.devflow/localbuild/
```

Global task cache:

```text
<os.UserCacheDir()>/devflow/cache/
```

The global cache is namespaced by project. Keep concurrency in mind when changing cache writes because multiple worktrees can publish the same task/key concurrently.

## Verification

Default:

```bash
go test ./...
```

Before handing off substantial changes, also run the checks enforced by CI:

```bash
go mod tidy -diff
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
```

Useful focused checks:

```bash
go test ./internal/cli
go test ./pkg/engine
go test ./pkg/cache ./pkg/instance
go test ./examples/go-next-monorepo
```

Build check:

```bash
go build -o "$(mktemp -d)/devflow" ./cmd/devflow
```

The full Docker-backed database integration suite is opt-in locally; it talks directly to the Engine API and does not invoke the `docker` executable. GitHub Actions separately requires the non-default-port remote Postgres clone and focused PostgreSQL 16/17/18 PostGIS spatial-and-persistence cases on native Linux amd64 and Linux arm64 (hosted macOS runners cannot run Docker Desktop):

```bash
DEVFLOW_E2E_DOCKER=1 go test ./pkg/database -run Docker -v
```

## Documentation Rules

Keep documentation lanes explicit:
- setup user docs explain how to add Devflow to another project and shape the pipeline
- development user docs explain CLI/TUI/operator usage after Devflow is integrated
- contributor docs explain how to change Devflow itself

Do not put contributor-only internals in the first page a project adopter needs to read. Link to internals when useful.

When behavior changes, update the subsystem doc in the same change:
- CLI contracts: `docs_contributors/cli.md`
- runtime/package boundaries: `docs_contributors/architecture.md`
- adapter API: `docs_users/adapter-guide.md`
- user setup workflow: `docs_users/setup.md`
- user development workflow: `docs_users/development.md`
- contributor workflow: `docs_contributors/README.md`
- tests: `docs_contributors/testing.md`
- durable cross-cutting project memory: `docs_contributors/agent-memory.md`

## Release Flow

Round 1 release is Go-first.

Install:

```bash
go install github.com/benjaco/devflow/cmd/devflow@latest
```

Update:

```bash
devflow upgrade
```

Push and pull-request validation runs quality/security checks, a Linux race-detector pass, Go 1.25 tests on Linux amd64, macOS 15 arm64, and Windows amd64, and a current-stable Go compatibility run on Linux. The matrix verifies its native architecture explicitly, making Apple Silicon a maintained CI target. Tag validation repeats the test/build workflow on all three operating systems. No binary artifacts, npm package, Homebrew tap, Scoop installer, GitHub API updater, or self-replacing executable are part of round 1.
