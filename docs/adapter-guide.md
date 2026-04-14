# Adapter Guide

Projects integrate with Devflow by implementing `pkg/project.Project`.

An adapter defines:
- tasks
- targets
- instance configuration

Tasks should stay semantic. The adapter decides which files, directories, env vars, and custom probes contribute to each fingerprint.

## Dependency Installation

Adapters can expose required tool commands through `DependencyProvider`.

Example:

```go
func (myProject) Dependencies() []project.Dependency {
    return []project.Dependency{
        {
            Name:    "sqlc",
            Command: "sqlc",
            Install: map[string]project.InstallScript{
                "darwin": {Script: "brew install sqlc"},
                "linux":  {Script: "go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest"},
            },
        },
    }
}
```

That enables:
- `devflow deps status --project my-project`
- `devflow deps install --project my-project`
- richer `doctor` output for missing prerequisites

Rules:
- `Command` should be the binary name Devflow can verify on `PATH`
- install scripts should be platform-specific and idempotent when practical
- installers should leave the command actually available on `PATH`, because Devflow re-checks the command after install

## Dotenv Loading

Adapters can now load `.env` files directly through `pkg/project`.

Example:

```go
dotenv, err := project.LoadOptionalDotEnvInWorktree(worktree, ".env")
if err != nil {
    return project.InstanceConfig{}, err
}

return project.InstanceConfig{
    Env: project.MergeEnvMaps(dotenv, map[string]string{
        "DEVFLOW_PROJECT": "my-project",
    }),
    Finalize: func(inst *api.Instance) error {
        inst.Env = project.MergeEnvMaps(inst.Env, map[string]string{
            "DATABASE_URL": computedDatabaseURL,
        })
        return nil
    },
}
```

Recommended precedence:
- `.env`
- adapter defaults
- devflow-owned runtime overrides

Use dotenv values for normal app configuration, but keep leased ports, instance IDs, and per-instance DB URLs under devflow control.

## Interactive Task Policy

Adapters should prefer non-interactive subprocesses.

Rules:
- for package installs and similar setup commands, prefer explicit confirmation flags such as `-y`, `--yes`, `--force`, or `CI=1` when the adapter has already decided the action is correct
- do not rely on hidden stdin prompts during normal `run` or `watch` targets
- if a task needs a real user choice, model it as an explicit command, target, or future TUI action instead of letting the process stall

This is especially important for detached runs and watch mode, where a blocked prompt is easy to miss and hard to recover from.

When a task truly does need prompt/answer interaction, the adapter can use interactive command specs instead of raw shell blocking:

```go
return rt.RunCmdSpec(ctx, process.CommandSpec{
    Name:        "my-tool",
    Args:        []string{"setup"},
    Interactive: true,
    Prompts: []process.PromptSpec{
        {Pattern: "Continue? [y/N]: ", Prompt: "Continue?", Kind: process.PromptConfirm},
        {Pattern: "Name: ", Prompt: "Name", Kind: process.PromptText},
    },
})
```

Semantics:
- Devflow watches subprocess output for the declared prompt patterns
- matching prompts become `interaction_requested` events
- detached runs can then be answered from the TUI
- the answer is written back to subprocess stdin and recorded as `interaction_answered`

This path should still be the exception, not the default adapter style.

### Prisma Guidance

Treat Prisma authoring and reset flows separately from normal startup.

Recommended split:
- normal DB prep:
  - restore the nearest DB snapshot
  - replay the remaining known migrations
- new migration authoring:
  - explicit action with a provided name
  - use `prisma migrate dev --name <name>` or `prisma migrate dev --name <name> --create-only`
- destructive reset:
  - explicit action only
  - use `prisma migrate reset --force`

Do not make normal boot targets depend on interactive `prisma migrate dev`. From Devflow's point of view, that command can still become non-deterministic when Prisma detects drift or wants a reset decision.

## Built Binary Tools

For repo-local helper executables, use the built-in binary-tool helper in `pkg/project`.

Example:

```go
tool := project.BinaryTool{
    TaskName: "build_auth_mapping",
    Inputs: project.Inputs{
        Files: []string{"tools/auth-mapping/main.go", "go.mod", "go.sum"},
    },
    Output: ".devflow/tools/auth-mapping",
    Build: process.CommandSpec{
        Name: "go",
        Args: []string{"build", "-o", ".devflow/tools/auth-mapping", "./tools/auth-mapping"},
    },
}
buildTask := tool.BuildTask()

tasks := []project.Task{
    buildTask,
    {
        Name: "auth_mapping",
        Kind: project.KindOnce,
        Deps: []string{buildTask.Name},
        Run: func(ctx context.Context, rt *project.Runtime) error {
            return tool.Run(ctx, rt, "--config", rt.Abs("backend/auth/config.json"))
        },
    },
}
```

Rules:
- the tool output path should be worktree-relative so it can be cached and restored
- keep the binary output outside the input directories you fingerprint, or ignore it explicitly
- use the task `Inputs` to describe what should invalidate the build
- use `Signature` if the build command itself needs a stable explicit version marker

This gives you a standard way to compile helper binaries once, cache them by input hash, and run the restored artifact later from downstream tasks.

## Service Readiness

Service tasks can define an optional readiness function.

Current shape:

```go
type ReadyFunc func(ctx context.Context, rt *Runtime) error

type Task struct {
    Name         string
    Kind         Kind
    Run          RunFunc
    Ready        ReadyFunc
    ReadyTimeout time.Duration
    // ...
}
```

Use readiness when process start is not the same as service usability.

Examples:
- wait for a TCP listener on a named port
- poll an HTTP health endpoint
- wait for a ready file or socket to appear
- combine multiple checks with `ReadyAll(...)`

Rules:
- readiness should be narrow and deterministic
- a readiness check should describe service usability, not broad system health
- if a readiness check is configured, the engine will fail the task if it times out or the process exits first
- tasks without a readiness check are considered ready immediately after process start

## Cache Key Overrides

By default, Devflow computes cache keys automatically from the task definition, selected inputs, env, and dependency keys.

For tasks with a better domain-specific notion of identity, the design allows a per-task cache-key override. This is intended for cases where the adapter can compute a more correct semantic key than generic file/env hashing.

Current shape:

```go
type CacheKeyFunc func(ctx context.Context, rt *Runtime) (string, error)

type Task struct {
    Name             string
    Kind             Kind
    Cache            bool
    CacheKeyOverride CacheKeyFunc
    // ...
}
```

Rules:
- only cacheable `KindOnce` tasks may use it
- the override replaces the automatic key body
- the engine should still salt the final key with engine version and task name
- override mode should be explicit and rare

Use it when:
- the task has a canonical artifact fingerprint already
- file hashing is too broad or too noisy
- correctness depends on semantic inputs that are easier to compute directly than to enumerate generically

Avoid it when:
- the generic input model is already sufficient
- the override would hide important dependency/config/version changes

If an override is used, the adapter owns correctness for that task’s cache identity.

The core engine does not know about Prisma, sqlc, Next.js, or any repo-specific conventions.
