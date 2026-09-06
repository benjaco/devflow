package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benjaco/devflow/internal/execution"
	"github.com/benjaco/devflow/pkg/planner"
	"github.com/benjaco/devflow/pkg/project"
)

type planningCLIProject struct {
	callbacks atomic.Int32
	declared  bool
	conflict  bool
}

func (*planningCLIProject) Name() string { return "cli-planning-project" }

func (p *planningCLIProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	p.callbacks.Add(1)
	return project.InstanceConfig{}, fmt.Errorf("planning must not configure an instance")
}

func (p *planningCLIProject) Tasks() []project.Task {
	callback := func(context.Context, *project.Runtime) error {
		p.callbacks.Add(1)
		return fmt.Errorf("planning must not execute callbacks")
	}
	fingerprint := func(context.Context, *project.Runtime) (string, error) {
		p.callbacks.Add(1)
		return "", fmt.Errorf("planning must not compute fingerprints")
	}
	tasks := []project.Task{
		{Name: "generate", Kind: project.KindOnce, Purposes: []project.Purpose{project.PurposeGenerate}, Tags: []string{"shared"}, Inputs: project.Inputs{Dirs: []string{"shared"}, Custom: []project.FingerprintFunc{fingerprint}}, Outputs: project.Outputs{Files: []string{"generated.go"}}, Run: callback},
		{Name: "frontend-check", Kind: project.KindOnce, Purposes: []project.Purpose{project.PurposeTest}, Tags: []string{"frontend"}, Deps: []string{"generate"}, Inputs: project.Inputs{Dirs: []string{"frontend"}, Ignore: []string{"generated"}}, RequiredCLIs: []string{"missing-tool"}, RequiredEnv: []string{"TEST_TOKEN"}, BeforeRun: callback, Run: callback, CacheKeyOverride: fingerprint},
		{Name: "backend-check", Kind: project.KindOnce, Purposes: []project.Purpose{project.PurposeLint}, Tags: []string{"backend"}, Deps: []string{"generate"}, Inputs: project.Inputs{Dirs: []string{"backend"}}, RequiredCLIs: []string{"missing-tool"}, RequiredEnv: []string{"TEST_TOKEN"}, Run: callback},
		{Name: "format", Kind: project.KindOnce, Purposes: []project.Purpose{project.PurposeFormat}, Inputs: project.Inputs{Dirs: []string{"frontend"}}, Run: callback},
		{Name: "serve", Kind: project.KindService, Inputs: project.Inputs{Dirs: []string{"frontend"}}, BeforeRun: callback, Run: callback, Ready: callback, AfterReady: callback},
	}
	if p.declared {
		for i := range tasks {
			tasks[i].Effects = &project.Effects{}
			tasks[i].Inputs.Custom = nil
			tasks[i].CacheKeyOverride = nil
		}
	}
	if p.conflict {
		for _, index := range []int{1, 2} {
			tasks[index].Effects = &project.Effects{Resources: []project.ResourceUse{{Name: "shared-database", Access: project.ResourceWrite}}}
		}
	}
	return tasks
}

func (*planningCLIProject) Targets() []project.Target {
	return []project.Target{{Name: "checks", RootTasks: []string{"frontend-check", "backend-check"}, Verification: true}}
}

func (*planningCLIProject) RequiredCLIs() []project.RequiredCLI {
	return []project.RequiredCLI{{Name: "missing-tool", Command: "devflow-planner-never-installed"}}
}

func (*planningCLIProject) RequiredEnvs() []string { return []string{"PROJECT_TOKEN"} }

func runPlanJSON(t *testing.T, args ...string) (map[string]json.RawMessage, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := &App{Stdout: &stdout, Stderr: &stderr}
	err := a.Run(args)
	decoder := json.NewDecoder(&stdout)
	var result map[string]json.RawMessage
	if decodeErr := decoder.Decode(&result); decodeErr != nil {
		t.Fatalf("command %v returned invalid JSON: %v (command error %v, stderr %q)", args, decodeErr, err, stderr.String())
	}
	var extra any
	if decodeErr := decoder.Decode(&extra); decodeErr != io.EOF {
		t.Fatalf("command %v returned more than one JSON document: %v", args, decodeErr)
	}
	if stderr.Len() != 0 {
		t.Fatalf("command %v printed plain stderr diagnostics: %q", args, stderr.String())
	}
	return result, err
}

func TestPlanJSONSelectsChecksWithoutRuntimeCallbacks(t *testing.T) {
	p := &planningCLIProject{}
	project.Register(p)
	root := t.TempDir()
	t.Setenv("TEST_TOKEN", "plan-secret-test-value")
	t.Setenv("PROJECT_TOKEN", "plan-secret-project-value")
	result, err := runPlanJSON(t, "plan", "--files", "frontend/page.tsx", "--project", p.Name(), "--worktree", root, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var closure []string
	if err := json.Unmarshal(result["closure"], &closure); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(closure, []string{"generate", "frontend-check"}) {
		t.Errorf("closure = %v; expected only the frontend check and its generator", closure)
	}
	if string(result["intent"]) != `"verify"` || len(result["graphDigest"]) < 10 {
		t.Errorf("missing intent or graph digest: %s", result)
	}
	var checks []planner.Check
	if err := json.Unmarshal(result["checks"], &checks); err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || !reflect.DeepEqual(checks[0].Command, []string{"devflow", "run", "frontend-check", "--ci", "--json", "--project", p.Name(), "--worktree", root}) {
		t.Errorf("proposed command lost project/worktree scope: %+v", checks)
	}
	if p.callbacks.Load() != 0 {
		t.Errorf("planning invoked %d runtime callbacks", p.callbacks.Load())
	}
	var prerequisites planner.Prerequisites
	if err := json.Unmarshal(result["prerequisites"], &prerequisites); err != nil {
		t.Fatal(err)
	}
	if prerequisites.Availability != "unchecked" || !reflect.DeepEqual(prerequisites.CLIs, []string{"missing-tool"}) || !reflect.DeepEqual(prerequisites.Env, []string{"PROJECT_TOKEN", "TEST_TOKEN"}) {
		t.Errorf("prerequisites = %+v", prerequisites)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("plan-secret-")) {
		t.Fatal("plan serialized environment values")
	}
	if _, err := os.Stat(filepath.Join(root, ".devflow")); !os.IsNotExist(err) {
		t.Errorf("planning created runtime state: %v", err)
	}
}

func TestPlanJSONRecordsChangeScopeAndSharedDependencies(t *testing.T) {
	p := &planningCLIProject{}
	project.Register(p)
	root := t.TempDir()
	first, err := runPlanJSON(t, "plan", "--files", "frontend/page.tsx", "--project", p.Name(), "--worktree", root, "--json")
	if err != nil {
		t.Fatal(err)
	}
	second, err := runPlanJSON(t, "plan", "--files", "frontend/page.tsx,backend/handler.go", "--project", p.Name(), "--worktree", root, "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first["graphDigest"], second["graphDigest"]) || bytes.Equal(first["scopeDigest"], second["scopeDigest"]) {
		t.Errorf("changed scope must change scopeDigest independently of graphDigest: first=%s second=%s", first, second)
	}
	var closure []string
	if err := json.Unmarshal(second["closure"], &closure); err != nil {
		t.Fatal(err)
	}
	if len(closure) != 3 || closure[0] != "generate" {
		t.Fatalf("expected two checks and one shared prerequisite: %v", closure)
	}
	var shared []planner.SharedDependency
	if err := json.Unmarshal(second["sharedDependencies"], &shared); err != nil {
		t.Fatal(err)
	}
	if len(shared) != 1 || shared[0].Task != "generate" || len(shared[0].Checks) != 2 {
		t.Errorf("shared dependencies = %+v", shared)
	}
}

func TestPlanJSONClassifiesIgnoredUnmatchedAndConfiguration(t *testing.T) {
	p := &planningCLIProject{}
	project.Register(p)
	for _, test := range []struct{ file, state string }{
		{"backend/handler.go", "matched"},
		{"README.md", "unmatched"},
		{"devflow.project.go", "configuration"},
		{"devflow_added.go", "configuration"},
		{"devflow_deleted.go", "configuration"},
		{"devflow_renamed.go", "configuration"},
		{"devflow_fixture_test.go", "unmatched"},
		{"main.go", "unmatched"},
		{"nested/devflow_helper.go", "unmatched"},
	} {
		t.Run(test.file, func(t *testing.T) {
			result, err := runPlanJSON(t, "plan", "--files", test.file, "--project", p.Name(), "--worktree", t.TempDir(), "--json")
			if err != nil {
				t.Fatal(err)
			}
			var impacts []planner.FileImpact
			if err := json.Unmarshal(result["fileImpacts"], &impacts); err != nil {
				t.Fatal(err)
			}
			if len(impacts) != 1 || impacts[0].State != test.state || impacts[0].File != test.file {
				t.Errorf("impacts = %+v, expected state %q", impacts, test.state)
			}
			if string(result["configurationChanged"]) != fmt.Sprint(test.state == "configuration") {
				t.Errorf("configurationChanged = %s", result["configurationChanged"])
			}
			if test.state == "configuration" {
				var checks []planner.Check
				if err := json.Unmarshal(result["checks"], &checks); err != nil {
					t.Fatal(err)
				}
				if len(checks) != 1 || checks[0].Name != "checks" || checks[0].Kind != "target" || len(checks[0].Closure) != 3 || len(checks[0].ValidationCommand) == 0 {
					t.Errorf("configuration change did not select the declared full verification target: %+v", checks)
				}
			}
		})
	}
	// This existing adapter declares the same directory with an explicit ignore,
	// without another task independently covering that ignored path.
	project.Register(graphExplainCLIProject{})
	result, err := runPlanJSON(t, "plan", "--files", "internal/storage/sqlc/queries.go", "--project", (graphExplainCLIProject{}).Name(), "--worktree", t.TempDir(), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var impacts []planner.FileImpact
	if err := json.Unmarshal(result["fileImpacts"], &impacts); err != nil {
		t.Fatal(err)
	}
	if len(impacts) != 1 || impacts[0].State != "ignored" {
		t.Errorf("ignored input was not distinguished from unmatched: %+v", impacts)
	}
}

func TestPlanJSONOwnershipSnapshotDoesNotAcquireOrChangeOwner(t *testing.T) {
	p := &planningCLIProject{}
	project.Register(p)
	root := t.TempDir()
	lease, err := execution.Acquire(root, execution.Owner{Target: "existing-development", Mode: "watch", Kind: "daemon"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Release(); err != nil {
			t.Error(err)
		}
	})
	ownerPath := filepath.Join(root, ".devflow", "execution-owner.json")
	before, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runPlanJSON(t, "plan", "--files", "frontend/page.tsx", "--project", p.Name(), "--worktree", root, "--json")
	if err != nil {
		t.Fatalf("advisory planning should inspect an occupied worktree: %v", err)
	}
	var state planExecution
	if err := json.Unmarshal(result["execution"], &state); err != nil {
		t.Fatal(err)
	}
	if !state.ExclusiveWorktree || state.Admission != "required" || state.Owner == nil || state.Owner.Token != lease.Owner().Token {
		t.Errorf("execution snapshot = %+v", state)
	}
	after, err := os.ReadFile(ownerPath)
	if err != nil || !bytes.Equal(before, after) || p.callbacks.Load() != 0 {
		t.Errorf("planning changed execution: owner read=%v callbacks=%d", err, p.callbacks.Load())
	}
}

func TestPlanJSONMalformedOwnerRetainsPlanAndTypedError(t *testing.T) {
	p := &planningCLIProject{}
	project.Register(p)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".devflow"), 0o700); err != nil {
		t.Fatal(err)
	}
	ownerPath := filepath.Join(root, ".devflow", "execution-owner.json")
	if err := os.WriteFile(ownerPath, []byte("broken owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := runPlanJSON(t, "plan", "--files", "frontend/page.tsx", "--project", p.Name(), "--worktree", root, "--json")
	if err == nil {
		t.Fatal("malformed ownership should be explicit")
	}
	var failure struct{ Code, Phase string }
	if err := json.Unmarshal(result["error"], &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != "ownership_read_failed" || failure.Phase != "planning" || len(result["checks"]) < 3 {
		t.Errorf("missing typed error or partial selection: %s", result)
	}
	contents, err := os.ReadFile(ownerPath)
	if err != nil || string(contents) != "broken owner" {
		t.Errorf("malformed ownership was changed: %q %v", contents, err)
	}
}

func TestPlanJSONEnrichmentErrorsLeaveResolvedFalse(t *testing.T) {
	t.Setenv(envLocalExec, "1")
	for _, scenario := range []string{"ownership", "configuration"} {
		t.Run(scenario, func(t *testing.T) {
			p := &planningCLIProject{declared: true}
			project.Register(p)
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, localProjectFile), []byte("package main\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"plan", "--files", "backend/handler.go", "--project", p.Name(), "--worktree", root, "--json"}
			before, err := runPlanJSON(t, args...)
			if err != nil || string(before["resolved"]) != "true" {
				t.Fatalf("fully declared baseline should resolve: %s %v", before, err)
			}
			if scenario == "ownership" {
				if err := os.Mkdir(filepath.Join(root, ".devflow"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, ".devflow", "execution-owner.json"), []byte("broken owner"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(filepath.Join(root, "devflow_invalid.go"), 0o700); err != nil {
				t.Fatal(err)
			}
			after, err := runPlanJSON(t, args...)
			if err == nil || string(after["resolved"]) != "false" || !bytes.Equal(before["checks"], after["checks"]) {
				t.Errorf("enrichment error claimed resolved or discarded checks: %s %v", after, err)
			}
		})
	}
}

func TestPlanTextShowsConflictsAndUncheckedPrerequisites(t *testing.T) {
	p := &planningCLIProject{declared: true, conflict: true}
	project.Register(p)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"plan", "--files", "shared/schema.json", "--project", p.Name(), "--worktree", t.TempDir()}); err != nil {
		t.Fatalf("text planning failed: %v, stderr=%s", err, stderr.String())
	}
	for _, want := range []string{"resolved: false", "unchecked", "missing-tool", "PROJECT_TOKEN", "TEST_TOKEN", "conflict:", "shared-database", "backend-check", "frontend-check"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("text plan omitted %q:\n%s", want, stdout.String())
		}
	}
}

func TestPlanJSONConfigurationIdentityTracksExactAdapterSources(t *testing.T) {
	t.Setenv(envLocalExec, "1")
	p := &planningCLIProject{}
	project.Register(p)
	root := t.TempDir()
	write := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(localProjectFile, "package main\n")
	read := func() map[string]json.RawMessage {
		t.Helper()
		result, err := runPlanJSON(t, "plan", "--files", "frontend/page.tsx", "--project", p.Name(), "--worktree", root, "--json")
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	initial := read()
	if len(initial["configDigest"]) < 10 {
		t.Fatalf("configuration identity is missing: %s", initial)
	}
	write("devflow_ignored_test.go", "not a runtime adapter source")
	write("unrelated.go", "not a runtime adapter source either")
	if ignored := read(); !bytes.Equal(initial["configDigest"], ignored["configDigest"]) {
		t.Error("excluded sources changed the configuration identity")
	}
	write("devflow_helper.go", "package main\n")
	added := read()
	if bytes.Equal(initial["configDigest"], added["configDigest"]) {
		t.Error("companion addition did not change configuration identity")
	}
	if err := os.Rename(filepath.Join(root, "devflow_helper.go"), filepath.Join(root, "devflow_renamed.go")); err != nil {
		t.Fatal(err)
	}
	renamed := read()
	if bytes.Equal(added["configDigest"], renamed["configDigest"]) {
		t.Error("same-content companion rename did not change configuration identity")
	}
	write("devflow_renamed.go", "package main\n// edited\n")
	if edited := read(); bytes.Equal(renamed["configDigest"], edited["configDigest"]) {
		t.Error("companion edit did not change configuration identity")
	}
	if err := os.Remove(filepath.Join(root, "devflow_renamed.go")); err != nil {
		t.Fatal(err)
	}
	if removed := read(); !bytes.Equal(initial["configDigest"], removed["configDigest"]) {
		t.Error("companion deletion did not restore the initial configuration identity")
	}
	if err := os.Mkdir(filepath.Join(root, "devflow_invalid.go"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := runPlanJSON(t, "plan", "--files", "frontend/page.tsx", "--project", p.Name(), "--worktree", root, "--json")
	if err == nil || !strings.Contains(string(result["error"]), `"adapter_source_invalid"`) {
		t.Errorf("invalid source did not return a typed source error: %s %v", result, err)
	}
}

func TestPlanJSONArgumentFailures(t *testing.T) {
	p := &planningCLIProject{}
	project.Register(p)
	root := t.TempDir()
	for _, test := range []struct {
		name  string
		args  []string
		code  string
		phase string
	}{
		{name: "missing files", args: []string{"plan", "--json"}, code: "invalid_arguments", phase: "parsing"},
		{name: "blank files", args: []string{"plan", "--files", " , ", "--json"}, code: "invalid_arguments", phase: "parsing"},
		{name: "unknown intent", args: []string{"plan", "--files", "x.go", "--intent", "deploy", "--json"}, code: "invalid_arguments", phase: "parsing"},
		{name: "unknown flag", args: []string{"plan", "--files", "x.go", "--bogus", "--json"}, code: "invalid_arguments", phase: "parsing"},
		{name: "extra argument", args: []string{"plan", "unexpected", "--files", "x.go", "--json"}, code: "invalid_arguments", phase: "parsing"},
		{name: "unknown project", args: []string{"plan", "--files", "x.go", "--project", "not-a-planning-project", "--json"}, code: "unknown_project", phase: "resolution"},
		{name: "rooted file", args: []string{"plan", "--files", "/absolute", "--project", p.Name(), "--worktree", root, "--json"}, code: "invalid_arguments", phase: "parsing"},
		{name: "native rooted file", args: []string{"plan", "--files", filepath.FromSlash("/absolute"), "--project", p.Name(), "--worktree", root, "--json"}, code: "invalid_arguments", phase: "parsing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := runPlanJSON(t, test.args...)
			if err == nil {
				t.Fatal("invalid command succeeded")
			}
			var failure struct{ Code, Phase, Message string }
			if err := json.Unmarshal(result["error"], &failure); err != nil {
				t.Fatal(err)
			}
			if failure.Code != test.code || failure.Phase != test.phase || failure.Message == "" || string(result["success"]) != "false" {
				t.Errorf("failure = %+v, result = %s", failure, result)
			}
		})
	}
}

func TestGraphShowJSONIncludesSafeMetadata(t *testing.T) {
	p := &planningCLIProject{}
	project.Register(p)
	result, err := runPlanJSON(t, "graph", "show", "checks", "--project", p.Name(), "--json")
	if err != nil {
		t.Fatal(err)
	}
	if string(result["target"]) != `"checks"` || len(result["closure"]) == 0 {
		t.Fatalf("graph show lost target or closure: %s", result)
	}
	var metadata struct {
		Digest string
		Tasks  []struct {
			Name         string
			HasRun       bool
			HasBeforeRun bool
			RequiredEnv  []string
			Tags         []string
		}
		Targets []struct{ Name string }
	}
	if err := json.Unmarshal(result["metadata"], &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Digest == "" || len(metadata.Tasks) != 3 || len(metadata.Targets) != 1 {
		t.Errorf("incomplete metadata: %+v", metadata)
	}
	for _, task := range metadata.Tasks {
		if !task.HasRun || (task.Name == "frontend-check" && !task.HasBeforeRun) {
			t.Errorf("callback presence was lost: %+v", task)
		}
		if task.Name == "frontend-check" && !reflect.DeepEqual(task.Tags, []string{"frontend"}) {
			t.Errorf("descriptive tags were lost: %+v", task)
		}
	}
	if p.callbacks.Load() != 0 {
		t.Errorf("graph metadata invoked %d runtime callbacks", p.callbacks.Load())
	}
}

func TestBootstrapPlanLoadsAdapterWithoutRuntimeCallbacks(t *testing.T) {
	isolateJSONContractState(t)
	root := t.TempDir()
	writeLocalProjectFile(t, root, planningBootstrapProjectSource)
	// Bootstrap and planning must both honor the explicit worktree even when
	// the invoking directory contains no adapter.
	stdout, stderr, err := runJSONContractCommand(t, t.TempDir(), "plan", "--worktree", root, "--files", "source.go", "--json")
	if err != nil {
		t.Fatalf("compiled planning failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	decoder := json.NewDecoder(strings.NewReader(stdout))
	var result planResult
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("decode compiled plan: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("compiled plan emitted additional JSON: %v", err)
	}
	if result.Result == nil || !result.Advisory || result.Resolved || result.ConfigDigest == "" || result.GraphDigest == "" || len(result.Checks) != 1 || result.Checks[0].Name != "check" || !reflect.DeepEqual(result.Closure, []string{"check"}) {
		t.Errorf("compiled plan lost declared verification metadata: %+v", result)
	}
	if len(result.Issues) != 1 || result.Issues[0].Code != "opaque_inputs" {
		t.Errorf("unevaluated fingerprints must retain input uncertainty: %+v", result.Issues)
	}
	if len(result.Prerequisites.CLIs) != 1 || result.Prerequisites.Availability != "unchecked" {
		t.Errorf("compiled plan checked or omitted unavailable prerequisite: %+v", result.Prerequisites)
	}
	for _, path := range []string{"state", "logs", "execution.lock", "execution-owner.json"} {
		if _, err := os.Stat(filepath.Join(root, ".devflow", path)); !os.IsNotExist(err) {
			t.Errorf("compiled planning created runtime %s: %v", path, err)
		}
	}
}

const planningBootstrapProjectSource = `package main

import (
	"context"
	"github.com/benjaco/devflow/pkg/project"
)

type planningBootstrapProject struct{}

func (planningBootstrapProject) Name() string { return "bootstrap-plan" }
func (planningBootstrapProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	panic("planning invoked ConfigureInstance")
}
func (planningBootstrapProject) Tasks() []project.Task {
	callback := func(context.Context, *project.Runtime) error { panic("planning invoked a runtime callback") }
	fingerprint := func(context.Context, *project.Runtime) (string, error) { panic("planning invoked a fingerprint") }
	return []project.Task{{
		Name: "check", Kind: project.KindOnce, Purposes: []project.Purpose{project.PurposeTest},
		Effects: &project.Effects{}, RequiredCLIs: []string{"unavailable"},
		Inputs: project.Inputs{Files: []string{"source.go"}, Custom: []project.FingerprintFunc{fingerprint}},
		BeforeRun: callback, Run: callback, CacheKeyOverride: fingerprint,
	}}
}
func (planningBootstrapProject) Targets() []project.Target {
	return []project.Target{{Name: "verify", RootTasks: []string{"check"}, Verification: true}}
}
func (planningBootstrapProject) RequiredCLIs() []project.RequiredCLI {
	return []project.RequiredCLI{{Name: "unavailable", Command: "devflow-never-installed-planning-tool"}}
}
func init() { project.Register(planningBootstrapProject{}) }
`
