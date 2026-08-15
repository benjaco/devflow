package validation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/project"
)

type validationTestProject struct {
	name    string
	tasks   []project.Task
	targets []project.Target
}

func (p validationTestProject) Name() string { return p.name }

func (p validationTestProject) Tasks() []project.Task { return p.tasks }

func (p validationTestProject) Targets() []project.Target { return p.targets }

func (p validationTestProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{}, nil
}

func TestArtifactValidationUsesOnlyDeclaredInputsAndDependencyOutputs(t *testing.T) {
	worktree := t.TempDir()
	writeValidationFile(t, worktree, "source.txt", "hello")
	p := validationTestProject{
		name: "artifacts-pass",
		tasks: []project.Task{
			{
				Name:    "generate",
				Kind:    project.KindOnce,
				Inputs:  project.Inputs{Files: []string{"source.txt"}},
				Outputs: project.Outputs{Files: []string{"generated.txt"}},
				Run: func(_ context.Context, rt *project.Runtime) error {
					if rt.Mode != api.ModeValidation || rt.Env["DEVFLOW_VALIDATION"] != "1" || rt.Env["DEVFLOW_VALIDATION_MODE"] != "artifacts" {
						return errors.New("missing validation runtime metadata")
					}
					data, err := os.ReadFile(rt.Abs("source.txt"))
					if err != nil {
						return err
					}
					return project.WriteFile(rt, "generated.txt", append(data, '!'), 0o644)
				},
			},
			{
				Name:    "package",
				Kind:    project.KindOnce,
				Deps:    []string{"generate"},
				Outputs: project.Outputs{Files: []string{"package.txt"}},
				Run: func(_ context.Context, rt *project.Runtime) error {
					data, err := os.ReadFile(rt.Abs("generated.txt"))
					if err != nil {
						return err
					}
					return project.WriteFile(rt, "package.txt", data, 0o644)
				},
			},
		},
		targets: []project.Target{{Name: "build", RootTasks: []string{"package"}}},
	}
	result := runValidation(t, p, worktree, api.ValidationModeArtifacts, DefaultMaxOrders)
	if !result.Success || result.Artifacts == nil || !result.Artifacts.Success {
		t.Fatalf("expected artifact validation success, got %+v", result)
	}
	if len(result.Artifacts.Tasks) != 2 {
		t.Fatalf("expected two task results, got %+v", result.Artifacts.Tasks)
	}
	if got := result.Artifacts.Tasks[0].DeclaredInputs; !reflect.DeepEqual(got, []string{"file:source.txt"}) {
		t.Fatalf("unexpected first task declarations: %v", got)
	}
	if got := result.Artifacts.Tasks[0].MaterializedInputs; !reflect.DeepEqual(got, []string{"source.txt"}) {
		t.Fatalf("unexpected first task inputs: %v", got)
	}
	if got := result.Artifacts.Tasks[0].ProducedOutputs; !reflect.DeepEqual(got, []string{"generated.txt"}) {
		t.Fatalf("unexpected first task outputs: %v", got)
	}
	if got := result.Artifacts.Tasks[1].DependencyOutputs; !reflect.DeepEqual(got, []string{"generated.txt"}) {
		t.Fatalf("unexpected dependency outputs: %v", got)
	}
	if _, err := os.Stat(filepath.Join(worktree, "generated.txt")); !os.IsNotExist(err) {
		t.Fatalf("validation mutated the real worktree: %v", err)
	}
}

func TestArtifactValidationFindsUndeclaredAndMissingOutputs(t *testing.T) {
	worktree := t.TempDir()
	writeValidationFile(t, worktree, "source.txt", "hello")
	p := validationTestProject{
		name: "artifacts-output-failures",
		tasks: []project.Task{{
			Name:    "generate",
			Kind:    project.KindOnce,
			Inputs:  project.Inputs{Files: []string{"source.txt"}},
			Outputs: project.Outputs{Files: []string{"expected.txt"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				return project.WriteFile(rt, "surprise.txt", []byte("wrong"), 0o644)
			},
		}},
		targets: []project.Target{{Name: "build", RootTasks: []string{"generate"}}},
	}
	result := runValidation(t, p, worktree, api.ValidationModeArtifacts, DefaultMaxOrders)
	if result.Success || result.Artifacts == nil || result.Artifacts.Success {
		t.Fatalf("expected artifact validation failure, got %+v", result)
	}
	task := result.Artifacts.Tasks[0]
	if !reflect.DeepEqual(task.UndeclaredWrites, []string{"surprise.txt"}) {
		t.Fatalf("unexpected undeclared writes: %v", task.UndeclaredWrites)
	}
	if !reflect.DeepEqual(task.MissingOutputs, []string{"file:expected.txt"}) {
		t.Fatalf("unexpected missing outputs: %v", task.MissingOutputs)
	}
}

func TestArtifactValidationAllowsOutputParentsAndFindsEmptyUndeclaredDirs(t *testing.T) {
	worktree := t.TempDir()
	p := validationTestProject{
		name: "artifact-directories",
		tasks: []project.Task{{
			Name:    "generate",
			Kind:    project.KindOnce,
			Outputs: project.Outputs{Files: []string{"dist/out.txt"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				if err := project.WriteFile(rt, "dist/out.txt", []byte("output"), 0o644); err != nil {
					return err
				}
				return os.Mkdir(rt.Abs("scratch"), 0o755)
			},
		}},
		targets: []project.Target{{Name: "build", RootTasks: []string{"generate"}}},
	}
	result := runValidation(t, p, worktree, api.ValidationModeArtifacts, DefaultMaxOrders)
	if result.Success {
		t.Fatalf("expected undeclared directory failure")
	}
	task := result.Artifacts.Tasks[0]
	if !reflect.DeepEqual(task.UndeclaredWrites, []string{"scratch"}) {
		t.Fatalf("unexpected undeclared writes: %+v", task)
	}
}

func TestArtifactValidationRejectsExternalOutputSymlinks(t *testing.T) {
	worktree := t.TempDir()
	external := filepath.Join(t.TempDir(), "external.txt")
	writeValidationFile(t, filepath.Dir(external), filepath.Base(external), "outside")
	probe := filepath.Join(worktree, "symlink-probe")
	if err := os.Symlink(external, probe); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	p := validationTestProject{
		name: "artifact-external-symlink",
		tasks: []project.Task{{
			Name:    "generate",
			Kind:    project.KindOnce,
			Outputs: project.Outputs{Files: []string{"out.link"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				return os.Symlink(external, rt.Abs("out.link"))
			},
		}},
		targets: []project.Target{{Name: "build", RootTasks: []string{"generate"}}},
	}
	result := runValidation(t, p, worktree, api.ValidationModeArtifacts, DefaultMaxOrders)
	if result.Success || !strings.Contains(result.Artifacts.Tasks[0].Error, "outside the validation worktree") {
		t.Fatalf("expected external symlink failure, got %+v", result)
	}
}

func TestArtifactValidationFailsWhenUndeclaredWorktreeInputIsNeeded(t *testing.T) {
	worktree := t.TempDir()
	writeValidationFile(t, worktree, "declared.txt", "declared")
	writeValidationFile(t, worktree, "secret.txt", "secret")
	p := validationTestProject{
		name: "artifacts-input-failure",
		tasks: []project.Task{{
			Name:    "generate",
			Kind:    project.KindOnce,
			Inputs:  project.Inputs{Files: []string{"declared.txt"}},
			Outputs: project.Outputs{Files: []string{"out.txt"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				_, err := os.ReadFile(rt.Abs("secret.txt"))
				return err
			},
		}},
		targets: []project.Target{{Name: "build", RootTasks: []string{"generate"}}},
	}
	result := runValidation(t, p, worktree, api.ValidationModeArtifacts, DefaultMaxOrders)
	if result.Success {
		t.Fatalf("expected validation failure")
	}
	task := result.Artifacts.Tasks[0]
	if task.InputCheck != "failed" || !hasIssueKind(task.Issues, "task_failed_with_projected_inputs") {
		t.Fatalf("expected projected-input failure, got %+v", task)
	}
}

func TestArtifactValidationMaterializesGlobsAndAppliesDirectoryIgnores(t *testing.T) {
	worktree := t.TempDir()
	writeValidationFile(t, worktree, "src/used.txt", "used")
	writeValidationFile(t, worktree, "src/ignored.tmp", "ignored")
	writeValidationFile(t, worktree, "config.dev.json", "config")
	p := validationTestProject{
		name: "artifact-input-shapes",
		tasks: []project.Task{{
			Name: "generate",
			Kind: project.KindOnce,
			Inputs: project.Inputs{
				Dirs:   []string{"src"},
				Globs:  []string{"config.*.json"},
				Ignore: []string{"*.tmp"},
			},
			Outputs: project.Outputs{Files: []string{"out.txt"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				if _, err := os.Stat(rt.Abs("src/ignored.tmp")); !os.IsNotExist(err) {
					return errors.New("ignored input was materialized")
				}
				used, err := os.ReadFile(rt.Abs("src/used.txt"))
				if err != nil {
					return err
				}
				config, err := os.ReadFile(rt.Abs("config.dev.json"))
				if err != nil {
					return err
				}
				return project.WriteFile(rt, "out.txt", append(used, config...), 0o644)
			},
		}},
		targets: []project.Target{{Name: "build", RootTasks: []string{"generate"}}},
	}
	result := runValidation(t, p, worktree, api.ValidationModeArtifacts, DefaultMaxOrders)
	if !result.Success {
		t.Fatalf("expected validation success, got %+v", result)
	}
	want := []string{"config.dev.json", "src/used.txt"}
	if got := result.Artifacts.Tasks[0].MaterializedInputs; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected materialized inputs: got %v want %v", got, want)
	}
}

func TestArtifactValidationPreservesInternalPNPMSymlinkGraph(t *testing.T) {
	worktree := t.TempDir()
	modules := filepath.Join(worktree, "node_modules")
	pkg := filepath.Join(modules, ".pnpm", "pkg@1.0.0", "node_modules", "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	writeValidationFile(t, pkg, "index.js", "module.exports = 1\n")
	if err := os.Symlink(filepath.Join(".pnpm", "pkg@1.0.0", "node_modules", "pkg"), filepath.Join(modules, "pkg")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := validationTestProject{
		name: "artifact-pnpm-symlinks",
		tasks: []project.Task{{
			Name:    "inspect",
			Kind:    project.KindOnce,
			Inputs:  project.Inputs{Dirs: []string{"node_modules"}},
			Outputs: project.Outputs{Files: []string{"out.txt"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				info, err := os.Lstat(rt.Abs("node_modules/pkg"))
				if err != nil {
					return err
				}
				if info.Mode()&os.ModeSymlink == 0 {
					return errors.New("pnpm package symlink was dereferenced")
				}
				data, err := os.ReadFile(rt.Abs("node_modules/pkg/index.js"))
				if err != nil {
					return err
				}
				return project.WriteFile(rt, "out.txt", data, 0o644)
			},
		}},
		targets: []project.Target{{Name: "build", RootTasks: []string{"inspect"}}},
	}
	result := runValidation(t, p, worktree, api.ValidationModeArtifacts, DefaultMaxOrders)
	if !result.Success {
		t.Fatalf("expected pnpm projection success, got %+v", result)
	}
	if got := result.Artifacts.Tasks[0].MaterializedInputs; !reflect.DeepEqual(got, []string{
		"node_modules/.pnpm/pkg@1.0.0/node_modules/pkg/index.js",
		"node_modules/pkg",
	}) {
		t.Fatalf("unexpected materialized pnpm projection: %v", got)
	}
}

func TestOrderValidationRunsEveryValidOrder(t *testing.T) {
	worktree := t.TempDir()
	p := independentOutputProject()
	result := runValidation(t, p, worktree, api.ValidationModeOrders, DefaultMaxOrders)
	if !result.Success || result.Orders == nil || !result.Orders.Success {
		t.Fatalf("expected order validation success, got %+v", result)
	}
	if result.Orders.TotalOrders != 2 || len(result.Orders.Runs) != 2 || !result.Orders.Complete {
		t.Fatalf("expected two exhaustive orders, got %+v", result.Orders)
	}
	want := [][]string{{"a", "b", "finish"}, {"b", "a", "finish"}}
	got := [][]string{result.Orders.Runs[0].Tasks, result.Orders.Runs[1].Tasks}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected orders: got %v want %v", got, want)
	}
}

func TestOrderValidationPreservesProducerInPlaceInputs(t *testing.T) {
	worktree := t.TempDir()
	writeValidationFile(t, worktree, "version.txt", "v1")
	p := validationTestProject{
		name: "in-place",
		tasks: []project.Task{{
			Name:    "bump",
			Kind:    project.KindOnce,
			Inputs:  project.Inputs{Files: []string{"version.txt"}},
			Outputs: project.Outputs{Files: []string{"version.txt"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				data, err := os.ReadFile(rt.Abs("version.txt"))
				if err != nil {
					return err
				}
				return project.WriteFile(rt, "version.txt", append(data, '!'), 0o644)
			},
		}},
		targets: []project.Target{{Name: "build", RootTasks: []string{"bump"}}},
	}
	result := runValidation(t, p, worktree, api.ValidationModeOrders, DefaultMaxOrders)
	if !result.Success || result.Orders == nil || len(result.Orders.Runs) != 1 {
		t.Fatalf("expected in-place validation success, got %+v", result)
	}
}

func TestOrderValidationFindsMissingDependency(t *testing.T) {
	worktree := t.TempDir()
	p := validationTestProject{
		name: "missing-dependency",
		tasks: []project.Task{
			{
				Name:    "a",
				Kind:    project.KindOnce,
				Outputs: project.Outputs{Files: []string{"a.txt"}},
				Run: func(_ context.Context, rt *project.Runtime) error {
					return project.WriteFile(rt, "a.txt", []byte("a"), 0o644)
				},
			},
			{
				Name:    "b",
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
		},
		targets: []project.Target{{Name: "build", RootTasks: []string{"a", "b"}}},
	}
	result := runValidation(t, p, worktree, api.ValidationModeOrders, DefaultMaxOrders)
	if result.Success || result.Orders == nil || len(result.Orders.Runs) != 2 {
		t.Fatalf("expected an order failure, got %+v", result)
	}
	if result.Orders.Runs[0].Tasks[0] != "a" || !result.Orders.Runs[0].Success {
		t.Fatalf("expected a->b order to pass, got %+v", result.Orders.Runs[0])
	}
	if result.Orders.Runs[1].FailedTask != "b" || result.Orders.Runs[1].Success {
		t.Fatalf("expected b->a order to fail at b, got %+v", result.Orders.Runs[1])
	}
}

func TestOrderValidationComparesFinalArtifacts(t *testing.T) {
	worktree := t.TempDir()
	appendShared := func(value string) project.RunFunc {
		return func(_ context.Context, rt *project.Runtime) error {
			path := rt.Abs("shared.tmp")
			data, err := os.ReadFile(path)
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := project.WriteFile(rt, "shared.tmp", append(data, value...), 0o644); err != nil {
				return err
			}
			return project.WriteFile(rt, value+".txt", []byte(value), 0o644)
		}
	}
	p := validationTestProject{
		name: "order-output-mismatch",
		tasks: []project.Task{
			{Name: "a", Kind: project.KindOnce, Outputs: project.Outputs{Files: []string{"a.txt"}}, Run: appendShared("a")},
			{Name: "b", Kind: project.KindOnce, Outputs: project.Outputs{Files: []string{"b.txt"}}, Run: appendShared("b")},
			{
				Name:    "finish",
				Kind:    project.KindOnce,
				Deps:    []string{"a", "b"},
				Outputs: project.Outputs{Files: []string{"final.txt"}},
				Run: func(_ context.Context, rt *project.Runtime) error {
					data, err := os.ReadFile(rt.Abs("shared.tmp"))
					if err != nil {
						return err
					}
					return project.WriteFile(rt, "final.txt", data, 0o644)
				},
			},
		},
		targets: []project.Target{{Name: "build", RootTasks: []string{"finish"}}},
	}
	result := runValidation(t, p, worktree, api.ValidationModeOrders, DefaultMaxOrders)
	if result.Success || result.Orders == nil || len(result.Orders.Runs) != 2 {
		t.Fatalf("expected output mismatch, got %+v", result)
	}
	second := result.Orders.Runs[1]
	if second.Success || !reflect.DeepEqual(second.OutputDifferences, []string{"final.txt"}) {
		t.Fatalf("expected final artifact mismatch, got %+v", second)
	}
}

func TestOrderValidationRefusesPartialEnumeration(t *testing.T) {
	p := validationTestProject{
		name: "limit",
		tasks: []project.Task{
			{Name: "a", Kind: project.KindOnce},
			{Name: "b", Kind: project.KindOnce},
			{Name: "c", Kind: project.KindOnce},
		},
		targets: []project.Target{{Name: "build", RootTasks: []string{"a", "b", "c"}}},
	}
	result := runValidation(t, p, t.TempDir(), api.ValidationModeOrders, 2)
	if result.Success || result.Orders == nil || result.Orders.Complete || len(result.Orders.Runs) != 0 {
		t.Fatalf("expected an unexecuted limit failure, got %+v", result)
	}
	if result.Orders.DiscoveredOrders != 3 || !hasIssueKind(result.Orders.Issues, "order_limit_exceeded") {
		t.Fatalf("unexpected limit result: %+v", result.Orders)
	}
}

func TestValidationRejectsServiceTargetsAndOutputCollisions(t *testing.T) {
	p := validationTestProject{
		name: "preflight",
		tasks: []project.Task{
			{Name: "a", Kind: project.KindOnce, Outputs: project.Outputs{Dirs: []string{"dist"}}},
			{Name: "b", Kind: project.KindOnce, Outputs: project.Outputs{Files: []string{"dist/b.txt"}}},
			{Name: "cached", Kind: project.KindOnce, Cache: true},
			{Name: "service", Kind: project.KindService, Deps: []string{"a", "b", "cached"}},
		},
		targets: []project.Target{{Name: "up", RootTasks: []string{"service"}}},
	}
	result := runValidation(t, p, t.TempDir(), api.ValidationModeAll, DefaultMaxOrders)
	if result.Success || result.Artifacts != nil || result.Orders != nil {
		t.Fatalf("expected preflight failure, got %+v", result)
	}
	if !hasIssueKind(result.Issues, "unsupported_task_kind") || !hasIssueKind(result.Issues, "output_collision") {
		t.Fatalf("expected service and collision issues, got %+v", result.Issues)
	}
	if !hasIssueKind(result.Issues, "missing_output_declaration") {
		t.Fatalf("expected cache output declaration issue, got %+v", result.Issues)
	}
}

func TestEnumerateTopologicalOrdersHonorsDependencies(t *testing.T) {
	tasks := []project.Task{
		{Name: "a", Kind: project.KindOnce},
		{Name: "b", Kind: project.KindOnce},
		{Name: "c", Kind: project.KindOnce, Deps: []string{"a", "b"}},
	}
	g, err := graph.New(tasks, []project.Target{{Name: "build", RootTasks: []string{"c"}}})
	if err != nil {
		t.Fatal(err)
	}
	orders, exceeded := enumerateTopologicalOrders(g, []string{"a", "b", "c"}, 10)
	if exceeded {
		t.Fatalf("did not expect limit to be exceeded")
	}
	want := [][]string{{"a", "b", "c"}, {"b", "a", "c"}}
	if !reflect.DeepEqual(orders, want) {
		t.Fatalf("unexpected topological orders: got %v want %v", orders, want)
	}
}

func independentOutputProject() validationTestProject {
	writeTask := func(name string) project.Task {
		return project.Task{
			Name:    name,
			Kind:    project.KindOnce,
			Outputs: project.Outputs{Files: []string{name + ".txt"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				if rt.Mode != api.ModeValidation || rt.Env["DEVFLOW_VALIDATION_MODE"] != "orders" {
					return errors.New("missing order validation runtime metadata")
				}
				return project.WriteFile(rt, name+".txt", []byte(name), 0o644)
			},
		}
	}
	return validationTestProject{
		name: "orders-pass",
		tasks: []project.Task{
			writeTask("a"),
			writeTask("b"),
			{
				Name:    "finish",
				Kind:    project.KindOnce,
				Deps:    []string{"a", "b"},
				Outputs: project.Outputs{Files: []string{"final.txt"}},
				Run: func(_ context.Context, rt *project.Runtime) error {
					return project.WriteFile(rt, "final.txt", []byte("done"), 0o644)
				},
			},
		},
		targets: []project.Target{{Name: "build", RootTasks: []string{"finish"}}},
	}
}

func runValidation(t *testing.T, p project.Project, worktree string, mode api.ValidationMode, maxOrders int) *api.ValidationResult {
	t.Helper()
	validator, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	result, err := validator.Run(context.Background(), Request{
		Target:    p.Targets()[0].Name,
		Worktree:  worktree,
		Mode:      mode,
		MaxOrders: maxOrders,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func writeValidationFile(t *testing.T, root, rel, value string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasIssueKind(issues []api.ValidationIssue, kind string) bool {
	for _, issue := range issues {
		if issue.Kind == kind {
			return true
		}
	}
	return false
}
