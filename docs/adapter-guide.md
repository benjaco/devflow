# Adapter Guide

Projects integrate with Devflow by implementing `pkg/project.Project`.

An adapter defines:
- tasks
- targets
- instance configuration

Tasks should stay semantic. The adapter decides which files, directories, env vars, and custom probes contribute to each fingerprint.

## Cache Key Overrides

By default, Devflow computes cache keys automatically from the task definition, selected inputs, env, and dependency keys.

For tasks with a better domain-specific notion of identity, the design allows a per-task cache-key override. This is intended for cases where the adapter can compute a more correct semantic key than generic file/env hashing.

Planned shape:

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
