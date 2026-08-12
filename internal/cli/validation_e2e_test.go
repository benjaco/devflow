package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
)

func TestBootstrapValidateDetectsProjectContractFailures(t *testing.T) {
	worktree := t.TempDir()
	writeLocalProjectFile(t, worktree, validationE2EProjectSource)
	if err := os.WriteFile(filepath.Join(worktree, "source.txt"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("artifact inputs and outputs", func(t *testing.T) {
		stdout, stderr, err := runBootstrapCommandCaptured(
			t,
			worktree,
			"validate", "artifact-contract",
			"--mode", "artifacts",
			"--json",
		)
		if err == nil {
			t.Fatalf("expected validation to exit non-zero, stdout:\n%s", stdout)
		}
		if !strings.Contains(stderr, "validation failed") {
			t.Fatalf("expected validation failure on stderr, got %q", stderr)
		}

		var result api.ValidationResult
		if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
			t.Fatalf("decode validation JSON: %v\nstdout:\n%s\nstderr:\n%s", decodeErr, stdout, stderr)
		}
		if result.Success || result.Artifacts == nil || result.Artifacts.Success {
			t.Fatalf("expected artifact validation failure, got %+v", result)
		}
		if len(result.Artifacts.Tasks) != 1 {
			t.Fatalf("expected one validated task, got %+v", result.Artifacts.Tasks)
		}
		task := result.Artifacts.Tasks[0]
		if !stringSliceContains(task.MaterializedInputs, "source.txt") {
			t.Fatalf("expected declared source input to be materialized, got %+v", task)
		}
		if !stringSliceContains(task.UndeclaredWrites, "surprise.txt") {
			t.Fatalf("expected undeclared output finding, got %+v", task)
		}
		if !stringSliceContains(task.MissingOutputs, "file:expected.txt") {
			t.Fatalf("expected missing output finding, got %+v", task)
		}
		assertValidationDidNotWrite(t, worktree, "surprise.txt", "expected.txt")
	})

	t.Run("all dependency-valid orders", func(t *testing.T) {
		stdout, stderr, err := runBootstrapCommandCaptured(
			t,
			worktree,
			"validate", "order-contract",
			"--mode", "orders",
			"--max-orders", "10",
			"--json",
		)
		if err == nil {
			t.Fatalf("expected validation to exit non-zero, stdout:\n%s", stdout)
		}
		if !strings.Contains(stderr, "validation failed") {
			t.Fatalf("expected validation failure on stderr, got %q", stderr)
		}

		var result api.ValidationResult
		if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil {
			t.Fatalf("decode validation JSON: %v\nstdout:\n%s\nstderr:\n%s", decodeErr, stdout, stderr)
		}
		if result.Success || result.Orders == nil || result.Orders.Success {
			t.Fatalf("expected task-order validation failure, got %+v", result)
		}
		if !result.Orders.Complete || result.Orders.TotalOrders != 2 || len(result.Orders.Runs) != 2 {
			t.Fatalf("expected both valid orders to run, got %+v", result.Orders)
		}
		var passingOrder, failingOrder bool
		for _, run := range result.Orders.Runs {
			if run.Success {
				passingOrder = true
			}
			if !run.Success && run.FailedTask == "b_consumer" {
				failingOrder = true
			}
		}
		if !passingOrder || !failingOrder {
			t.Fatalf("expected one passing order and one order failing at b_consumer, got %+v", result.Orders.Runs)
		}
		assertValidationDidNotWrite(t, worktree, "a.txt", "b.txt")
	})
}

func runBootstrapCommandCaptured(t *testing.T, worktree string, args ...string) (string, string, error) {
	t.Helper()
	repoRoot, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(buildBootstrapBinary(t), args...)
	cmd.Dir = worktree
	cmd.Env = withEnv(os.Environ(), envBootstrapEntry, "1")
	cmd.Env = withEnv(cmd.Env, envBootstrapRoot, repoRoot)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

func assertValidationDidNotWrite(t *testing.T, worktree string, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(filepath.Join(worktree, path)); !os.IsNotExist(err) {
			t.Fatalf("validation unexpectedly wrote %q to the real worktree: %v", path, err)
		}
	}
}

func stringSliceContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

const validationE2EProjectSource = `package main

import (
	"context"
	"os"

	"github.com/benjaco/devflow/pkg/project"
)

type validationE2EProject struct{}

func init() {
	project.Register(validationE2EProject{})
}

func (validationE2EProject) Name() string { return "validation-e2e-project" }

func (validationE2EProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "validation-e2e"}, nil
}

func (validationE2EProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:    "artifact_bad",
			Kind:    project.KindOnce,
			Inputs:  project.Inputs{Files: []string{"source.txt"}},
			Outputs: project.Outputs{Files: []string{"expected.txt"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				if _, err := os.ReadFile(rt.Abs("source.txt")); err != nil {
					return err
				}
				return project.WriteFile(rt, "surprise.txt", []byte("unexpected"), 0o644)
			},
		},
		{
			Name:    "a_producer",
			Kind:    project.KindOnce,
			Outputs: project.Outputs{Files: []string{"a.txt"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				return project.WriteFile(rt, "a.txt", []byte("a"), 0o644)
			},
		},
		{
			Name:    "b_consumer",
			Kind:    project.KindOnce,
			Outputs: project.Outputs{Files: []string{"b.txt"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				data, err := os.ReadFile(rt.Abs("a.txt"))
				if err != nil {
					return err
				}
				return project.WriteFile(rt, "b.txt", data, 0o644)
			},
		},
	}
}

func (validationE2EProject) Targets() []project.Target {
	return []project.Target{
		{Name: "artifact-contract", RootTasks: []string{"artifact_bad"}},
		{Name: "order-contract", RootTasks: []string{"a_producer", "b_consumer"}},
	}
}
`
