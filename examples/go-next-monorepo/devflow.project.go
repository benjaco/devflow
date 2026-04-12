package gonextmonorepo

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"

	"devflow/pkg/api"
	"devflow/pkg/instance"
	"devflow/pkg/process"
	"devflow/pkg/project"
)

type exampleProject struct{}

func init() {
	project.Register(exampleProject{})
}

func (exampleProject) Name() string {
	return "go-next-monorepo"
}

func (exampleProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	id, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		return project.InstanceConfig{}, err
	}
	return project.InstanceConfig{
		Label:     filepath.Base(worktree),
		PortNames: []string{"backend", "frontend"},
		Env: map[string]string{
			"DEVFLOW_EXAMPLE_PROJECT": "go-next-monorepo",
		},
		DB: api.DBInstance{
			Name: fmt.Sprintf("app_wt_%s", id),
		},
	}, nil
}

func (exampleProject) Tasks() []project.Task {
	return []project.Task{
		project.ShellTask(
			"warmup_node_install",
			"Placeholder warmup task",
			project.KindWarmup,
			nil,
			false,
			project.Outputs{},
			project.Inputs{},
			"mkdir -p .devflow/example && printf 'warmup\\n' > .devflow/example/warmup.txt",
		),
		{
			Name:        "backend_codegen",
			Kind:        project.KindOnce,
			Deps:        []string{"warmup_node_install"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"examples/go-next-monorepo/fixtures/backend/input.go"}},
			Outputs:     project.Outputs{Files: []string{".devflow/example/backend_codegen.json"}},
			Description: "Generate a mock backend artifact",
			Signature:   "backend-codegen-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				payload := map[string]any{
					"source":   rt.Abs("examples/go-next-monorepo/fixtures/backend/input.go"),
					"instance": rt.Instance.ID,
					"db":       rt.Instance.DB.Name,
				}
				data, err := json.MarshalIndent(payload, "", "  ")
				if err != nil {
					return err
				}
				return project.WriteFile(rt, ".devflow/example/backend_codegen.json", append(data, '\n'), 0o644)
			},
		},
		{
			Name:        "frontend_codegen",
			Kind:        project.KindOnce,
			Deps:        []string{"backend_codegen"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"examples/go-next-monorepo/fixtures/frontend/client.config.json"}},
			Outputs:     project.Outputs{Files: []string{".devflow/example/frontend_codegen.json"}},
			Description: "Generate a mock frontend client artifact",
			Signature:   "frontend-codegen-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				payload := map[string]any{
					"backendPort":  rt.Instance.Ports["backend"],
					"frontendPort": rt.Instance.Ports["frontend"],
					"instance":     rt.Instance.ID,
				}
				data, err := json.MarshalIndent(payload, "", "  ")
				if err != nil {
					return err
				}
				return project.WriteFile(rt, ".devflow/example/frontend_codegen.json", append(data, '\n'), 0o644)
			},
		},
		{
			Name:        "backend_dev",
			Kind:        project.KindService,
			Deps:        []string{"backend_codegen"},
			Description: "Run a mock backend service",
			Signature:   "backend-dev-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				env := map[string]string{}
				for key, value := range rt.Env {
					env[key] = value
				}
				env["BACKEND_PORT"] = strconv.Itoa(rt.Instance.Ports["backend"])
				_, err := rt.StartServiceSpec(ctx, process.CommandSpec{
					Name: "sh",
					Args: []string{"-c", "trap 'exit 0' INT TERM; while true; do echo backend:$BACKEND_PORT; sleep 1; done"},
					Dir:  rt.Worktree,
					Env:  env,
				})
				return err
			},
		},
		{
			Name:        "frontend_dev",
			Kind:        project.KindService,
			Deps:        []string{"frontend_codegen", "backend_dev"},
			Description: "Run a mock frontend service",
			Signature:   "frontend-dev-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				env := map[string]string{}
				for key, value := range rt.Env {
					env[key] = value
				}
				env["FRONTEND_PORT"] = strconv.Itoa(rt.Instance.Ports["frontend"])
				_, err := rt.StartServiceSpec(ctx, process.CommandSpec{
					Name: "sh",
					Args: []string{"-c", "trap 'exit 0' INT TERM; while true; do echo frontend:$FRONTEND_PORT; sleep 1; done"},
					Dir:  rt.Worktree,
					Env:  env,
				})
				return err
			},
		},
	}
}

func (exampleProject) Targets() []project.Target {
	return []project.Target{
		{
			Name:        "backend-codegen",
			RootTasks:   []string{"backend_codegen"},
			Description: "Run backend code generation",
		},
		{
			Name:        "frontend-stack",
			RootTasks:   []string{"frontend_dev"},
			Description: "Start the example frontend stack",
		},
		{
			Name:        "fullstack",
			RootTasks:   []string{"frontend_dev"},
			Description: "Alias for the full example stack",
		},
	}
}
