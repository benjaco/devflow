package main

import (
	"context"

	"devflow/pkg/project"
)

type localProject struct{}

func init() {
	project.Register(localProject{})
}

func (localProject) Name() string { return "local-rebuild-project" }

func (localProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "local"}, nil
}

func (localProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name: "noop",
			Kind: project.KindOnce,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				_ = rt
				return nil
			},
		},
	}
}

func (localProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"noop"}}}
}
