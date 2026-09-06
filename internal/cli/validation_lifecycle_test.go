package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

type validationLifecycleCLIProject struct{}

func init() { project.Register(validationLifecycleCLIProject{}) }

func (validationLifecycleCLIProject) Name() string { return "cli-validation-lifecycle" }

func (validationLifecycleCLIProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{}, nil
}

func (validationLifecycleCLIProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name: "hook-failure", Kind: project.KindOnce,
			BeforeRun: func(_ context.Context, rt *project.Runtime) error {
				rt.EmitLogLine("stderr", "hook diagnostic")
				return errors.New("hook rejected execution")
			},
			Run: func(_ context.Context, rt *project.Runtime) error {
				rt.EmitLogLine("stderr", "run must not execute")
				return nil
			},
		},
		{
			Name: "hook-only", Kind: project.KindOnce,
			Outputs: project.Outputs{Files: []string{"hook.txt"}},
			BeforeRun: func(_ context.Context, rt *project.Runtime) error {
				return project.WriteFile(rt, "hook.txt", []byte("hook output"), 0o644)
			},
		},
		{
			Name: "hook-env", Kind: project.KindOnce,
			Inputs:  project.Inputs{Files: []string{"source.txt"}},
			Outputs: project.Outputs{Files: []string{"env.txt"}},
			BeforeRun: func(_ context.Context, rt *project.Runtime) error {
				source, err := os.ReadFile(rt.Abs("source.txt"))
				if err != nil {
					return err
				}
				rt.Env["HOOK_VALUE"] = string(source)
				return nil
			},
			Run: func(_ context.Context, rt *project.Runtime) error {
				if rt.Env["HOOK_VALUE"] != "original source" {
					return fmt.Errorf("hook environment missing: %q", rt.Env["HOOK_VALUE"])
				}
				return project.WriteFile(rt, "env.txt", []byte(rt.Env["HOOK_VALUE"]), 0o644)
			},
		},
	}
}

func (validationLifecycleCLIProject) Targets() []project.Target {
	return []project.Target{
		{Name: "hook-failure", RootTasks: []string{"hook-failure"}},
		{Name: "hook-only", RootTasks: []string{"hook-only"}},
		{Name: "hook-env", RootTasks: []string{"hook-env"}},
	}
}

func TestValidateLifecycleJSON(t *testing.T) {
	for _, mode := range []string{"artifacts", "orders", "all"} {
		for _, target := range []string{"hook-failure", "hook-only", "hook-env"} {
			t.Run(mode+"/"+target, func(t *testing.T) {
				worktree := t.TempDir()
				if err := os.WriteFile(filepath.Join(worktree, "source.txt"), []byte("original source"), 0o644); err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				app := &App{Stdout: &stdout, Stderr: &stderr}
				err := app.Run([]string{
					"validate", target, "--mode", mode, "--details", "full", "--json",
					"--project", "cli-validation-lifecycle", "--worktree", worktree,
				})
				var result api.ValidationResult
				if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
					t.Fatalf("decode validation JSON: %v\nstdout: %s\nstderr: %s", decodeErr, &stdout, &stderr)
				}
				wantSuccess := target != "hook-failure"
				if (err == nil) != wantSuccess || result.Success != wantSuccess {
					t.Errorf("success = %v, command error = %v; want success = %v; issues = %+v", result.Success, err, wantSuccess, result.Issues)
				}
				if result.Target != target || string(result.Mode) != mode {
					t.Errorf("result target/mode = %q/%q, want %q/%q", result.Target, result.Mode, target, mode)
				}
				if (result.Artifacts != nil) != (mode != "orders") || (result.Orders != nil) != (mode != "artifacts") {
					t.Fatalf("unexpected validation sections for mode %q", mode)
				}
				if result.Artifacts != nil {
					if len(result.Artifacts.Tasks) != 1 {
						t.Fatalf("artifact tasks = %+v, want one task", result.Artifacts.Tasks)
					}
					task := result.Artifacts.Tasks[0]
					if result.Artifacts.Success != wantSuccess || task.Success != wantSuccess || task.Task != target {
						t.Errorf("artifact success = %v, task = %q/%v; want %q/%v", result.Artifacts.Success, task.Task, task.Success, target, wantSuccess)
					}
					if !wantSuccess {
						assertValidationHookFailure(t, task.Error, task.Log)
					} else if task.ProducedPathCount != 1 || task.MissingOutputCount != 0 || task.UndeclaredWriteCount != 0 {
						t.Errorf("hook output contract = %+v, want one declared output", task)
					}
				}
				if result.Orders != nil {
					if !result.Orders.Complete || len(result.Orders.Runs) != 1 {
						t.Fatalf("orders = %+v, want one complete order", result.Orders)
					}
					run := result.Orders.Runs[0]
					if result.Orders.Success != wantSuccess || run.Success != wantSuccess {
						t.Errorf("orders success = %v, run success = %v; want %v", result.Orders.Success, run.Success, wantSuccess)
					}
					if !wantSuccess {
						if run.FailedTask != target {
							t.Errorf("failed task = %q, want %q", run.FailedTask, target)
						}
						assertValidationHookFailure(t, run.Error, run.Log)
					} else if run.OutputDigest == "" {
						t.Error("successful order lacks artifact digest")
					}
				}
				assertValidationDidNotWrite(t, worktree, "hook.txt", "env.txt")
				source, err := os.ReadFile(filepath.Join(worktree, "source.txt"))
				if err != nil || string(source) != "original source" {
					t.Errorf("source changed: contents = %q, error = %v", source, err)
				}
			})
		}
	}
}

func assertValidationHookFailure(t *testing.T, taskError, log string) {
	t.Helper()
	if !strings.Contains(taskError, "hook rejected execution") || !strings.Contains(log, "hook diagnostic") {
		t.Errorf("missing hook failure evidence: error = %q, log = %q", taskError, log)
	}
	if strings.Contains(log, "run must not execute") {
		t.Errorf("Run executed after failing BeforeRun: %q", log)
	}
}
