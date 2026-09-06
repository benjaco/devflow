package planner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/project"
)

type fixture struct {
	tasks   []project.Task
	targets []project.Target
	actions []project.Action
}

func (fixture) Name() string                { return "planner-test" }
func (p fixture) Tasks() []project.Task     { return p.tasks }
func (p fixture) Targets() []project.Target { return p.targets }
func (p fixture) Actions() []project.Action { return p.actions }
func (fixture) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	panic("planner configured instance")
}
func (fixture) RequiredEnvs() []string { return []string{"TOKEN"} }
func (fixture) RequiredCLIs() []project.RequiredCLI {
	return []project.RequiredCLI{{Name: "go", Command: "must-not-execute"}}
}

func sample() fixture {
	forbidden := func(context.Context, *project.Runtime) error { panic("planner executed callback") }
	return fixture{tasks: []project.Task{
		{Name: "generate", Kind: project.KindOnce, Inputs: project.Inputs{Files: []string{"schema.json"}}, Outputs: project.Outputs{Files: []string{"generated/client.go"}}, Purposes: []project.Purpose{project.PurposeGenerate}, Effects: &project.Effects{Writes: []string{"generated/client.go"}}, Run: forbidden},
		{Name: "frontend", Kind: project.KindOnce, Deps: []string{"generate"}, Inputs: project.Inputs{Dirs: []string{"frontend"}, Ignore: []string{"ignored"}}, RequiredCLIs: []string{"go"}, RequiredEnv: []string{"TOKEN", "FRONTEND"}, Purposes: []project.Purpose{project.PurposeTest}, Effects: &project.Effects{}, BeforeRun: forbidden, Run: forbidden},
		{Name: "backend", Kind: project.KindOnce, Deps: []string{"generate"}, Inputs: project.Inputs{Dirs: []string{"backend"}}, RequiredCLIs: []string{"go"}, Purposes: []project.Purpose{project.PurposeLint}, Effects: &project.Effects{}, Run: forbidden},
		{Name: "format", Kind: project.KindOnce, Inputs: project.Inputs{Dirs: []string{"frontend"}, Ignore: []string{"ignored"}}, Purposes: []project.Purpose{project.PurposeFormat}, Effects: &project.Effects{Writes: []string{"frontend"}}, Run: forbidden},
		{Name: "test_named_but_undeclared", Kind: project.KindOnce, Inputs: project.Inputs{Dirs: []string{"mystery"}}, Run: forbidden},
	}, targets: []project.Target{{Name: "verify-all", RootTasks: []string{"frontend", "backend"}, Verification: true, RequiredEnv: []string{"VERIFY"}}}}
}

func names(r *Result) []string {
	out := []string{}
	for _, c := range r.Checks {
		out = append(out, c.Name)
	}
	return out
}
func issue(r *Result, code string) bool {
	for _, i := range r.Issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

func TestSelectNarrowChecksAndSharedDependencies(t *testing.T) {
	p := sample()
	r, err := Build(p, Request{Files: []string{"./frontend/app.ts", "frontend/app.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names(r), []string{"frontend"}) || !reflect.DeepEqual(r.Closure, []string{"generate", "frontend"}) {
		t.Fatalf("wrong checks: %+v", r)
	}
	if !r.Advisory || !r.Resolved || r.GraphDigest == "" || r.ScopeDigest == "" {
		t.Fatalf("missing planning contract: %+v", r)
	}
	if !reflect.DeepEqual(r.Prerequisites.CLIs, []string{"go"}) || !reflect.DeepEqual(r.Prerequisites.Env, []string{"FRONTEND", "TOKEN"}) || r.Prerequisites.Availability != "unchecked" {
		t.Fatalf("prerequisites: %+v", r.Prerequisites)
	}
	r, err = Build(p, Request{Files: []string{"schema.json"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names(r), []string{"backend", "frontend"}) || len(r.Closure) != 3 || len(r.SharedDependencies) != 1 || r.SharedDependencies[0].Task != "generate" {
		t.Fatalf("shared dependency lost: %+v", r)
	}
	if len(r.Checks[0].Command) == 0 || r.Checks[0].Command[0] != "devflow" {
		t.Fatal("missing execution argv")
	}
}

func TestClassifyConfigurationIgnoredAndUnmatched(t *testing.T) {
	for _, path := range []string{"devflow.project.go", "devflow_frontend.go", "./devflow_deleted.go"} {
		r, err := Build(sample(), Request{Files: []string{path}})
		if err != nil {
			t.Fatal(err)
		}
		if !r.ConfigurationChanged || !reflect.DeepEqual(names(r), []string{"verify-all"}) || !slices.Contains(r.Prerequisites.Env, "VERIFY") {
			t.Fatalf("configuration fallback: %+v", r)
		}
	}
	r, err := Build(sample(), Request{Files: []string{"devflow_unit_test.go", "nested/devflow_child.go", "README.md", "frontend/ignored/x.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.ConfigurationChanged || !issue(r, "unmatched_file") || r.Resolved || r.FileImpacts[3].State != "unmatched" {
		t.Fatalf("classification: %+v", r)
	}
	for _, f := range r.FileImpacts {
		if f.File == "frontend/ignored/x.ts" && f.State != "ignored" {
			t.Fatalf("ignore lost: %+v", f)
		}
	}
	p := sample()
	p.targets = nil
	r, err = Build(p, Request{Files: []string{"devflow_new.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Checks) != 0 || !issue(r, "configuration_impact_unknown") {
		t.Fatalf("invented config coverage: %+v", r)
	}
}

func TestUnknownMetadataAndUnsafeChecksRemainUnresolved(t *testing.T) {
	p := sample()
	p.targets = nil
	r, err := Build(p, Request{Files: []string{"mystery/file.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Checks) != 0 || !issue(r, "no_verification_check") {
		t.Fatalf("guessed purpose from name: %+v", r)
	}
	p = sample()
	p.tasks[1].Effects = nil
	p.tasks[1].Inputs.Custom = []project.FingerprintFunc{func(context.Context, *project.Runtime) (string, error) { panic("custom fingerprint executed") }}
	r, err = Build(p, Request{Files: []string{"frontend/app.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Resolved || !issue(r, "unknown_effects") || !issue(r, "opaque_inputs") {
		t.Fatalf("unknowns hidden: %+v", r)
	}
	p = sample()
	p.tasks[1].Deps = append(p.tasks[1].Deps, "format")
	r, err = Build(p, Request{Files: []string{"frontend/app.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Checks) != 0 || !issue(r, "unsafe_verification") {
		t.Fatalf("selected formatting dependency: %+v", r)
	}
	p = sample()
	p.actions = []project.Action{{ID: "migrate", Task: "frontend", Category: project.ActionCategoryAuthoring}}
	r, err = Build(p, Request{Files: []string{"frontend/app.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names(r), "frontend") {
		t.Fatalf("authoring action became verification: %+v", r)
	}
}

func TestDeclaredResourceConflictsRespectDependencies(t *testing.T) {
	p := sample()
	for _, i := range []int{1, 2} {
		p.tasks[i].Effects = &project.Effects{Resources: []project.ResourceUse{{Name: "database:test", Access: project.ResourceWrite}}}
	}
	r, err := Build(p, Request{Files: []string{"schema.json"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Conflicts) != 1 || r.Conflicts[0].Resource != "database:test" || r.Resolved {
		t.Fatalf("resource conflict hidden: %+v", r)
	}
	p.tasks[1].Deps = append(p.tasks[1].Deps, "backend")
	r, err = Build(p, Request{Files: []string{"schema.json"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Conflicts) != 0 {
		t.Fatalf("ordered resource use is not concurrent: %+v", r.Conflicts)
	}
}

func TestScopeDigestDeterminismAndInvalidFiles(t *testing.T) {
	a, _ := Build(sample(), Request{Files: []string{"frontend/a", "backend/b"}})
	b, _ := Build(sample(), Request{Files: []string{"backend/b", "./frontend/a", filepath.Join("frontend", "a"), "backend/b"}})
	c, _ := Build(sample(), Request{Files: []string{"frontend/a", "backend/b", "new.txt"}})
	if a.ScopeDigest == "" || a.ScopeDigest != b.ScopeDigest || a.ScopeDigest == c.ScopeDigest {
		t.Fatal("scope must track complete normalized change set")
	}
	bytes, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bytes), "must-not-execute") {
		t.Fatal("serialized command configuration")
	}
	for _, file := range []string{
		"../outside", "nested/../frontend/a", "/", "/absolute", "//server/share/file", "C:/absolute", "C:relative", ".",
		filepath.FromSlash("/absolute"), filepath.FromSlash("//server/share/file"),
		filepath.FromSlash("../outside"), filepath.FromSlash("nested/../frontend/a"),
	} {
		if _, err := Build(sample(), Request{Files: []string{file}}); err == nil {
			t.Errorf("accepted %q", file)
		}
	}
	if _, err := Build(sample(), Request{Files: []string{"x"}, Intent: "format"}); err == nil {
		t.Fatal("unsupported intent accepted")
	}
}

func TestTaskTargetCollisionDoesNotRecommendDifferentClosure(t *testing.T) {
	p := sample()
	p.targets = append(p.targets, project.Target{Name: "frontend", RootTasks: []string{"format"}})
	r, err := Build(p, Request{Files: []string{"frontend/app.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names(r), "frontend") {
		t.Fatalf("run frontend would select a different target: %+v", r)
	}
	if !issue(r, "ambiguous_execution_name") {
		t.Fatalf("collision hidden: %+v", r)
	}
}

func TestMixedConfigurationReasonsAreDeterministic(t *testing.T) {
	p := sample()
	p.targets = append(p.targets, project.Target{Name: "verify-other", RootTasks: []string{"frontend"}, Verification: true})
	var expected string
	for i := 0; i < 40; i++ {
		r, err := Build(p, Request{Files: []string{"frontend/app.ts", "devflow.project.go"}})
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			expected = string(data)
		} else if expected != string(data) {
			t.Fatal("identical mixed change scope produced different plan evidence")
		}
		for _, f := range r.FileImpacts {
			if f.File == "frontend/app.ts" && len(f.Checks) != 2 {
				t.Fatalf("not all full targets retained input reasons: %+v", f)
			}
		}
	}
}

func TestFileWriteReadConflictsRespectDependencyOrdering(t *testing.T) {
	p := sample()
	p.tasks[2].Inputs.Files = []string{"generated/client.go"}
	p.tasks[2].Deps = nil
	r, err := Build(p, Request{Files: []string{"frontend/app.ts", "backend/app.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Conflicts) != 1 || r.Conflicts[0].Resource != "file:generated/client.go" || r.Resolved {
		t.Fatalf("unordered generated input read was not reported: %+v", r.Conflicts)
	}
	p.tasks[2].Deps = []string{"generate"}
	r, err = Build(p, Request{Files: []string{"frontend/app.ts", "backend/app.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Conflicts) != 0 {
		t.Fatalf("dependency orders shared file use: %+v", r.Conflicts)
	}
}

func TestSharedInputBranchesRequireCoverage(t *testing.T) {
	p := sample()
	p.tasks[2].Inputs.Dirs = []string{"frontend"}
	p.tasks[2].Purposes = nil
	p.targets = []project.Target{{Name: "verify-backend", RootTasks: []string{"backend"}, Verification: true}}
	r, err := Build(p, Request{Files: []string{"frontend/app.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names(r), []string{"frontend", "verify-backend"}) {
		t.Fatalf("one check hid an uncovered input branch: %+v", r)
	}
	p.targets = nil
	r, err = Build(p, Request{Files: []string{"frontend/app.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Resolved || !issue(r, "uncovered_impact") {
		t.Fatalf("missing branch coverage claimed resolved: %+v", r)
	}
}

func TestFullTargetRetainsUntypedSourceReasons(t *testing.T) {
	p := sample()
	p.tasks[1].Purposes = nil
	p.tasks[2].Purposes = nil
	r, err := Build(p, Request{Files: []string{"devflow.project.go", "frontend/app.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Resolved {
		t.Fatalf("full configuration target already covers source: %+v", r.Issues)
	}
	for _, f := range r.FileImpacts {
		if len(f.Checks) == 0 {
			t.Fatalf("lost reason for covered file: %+v", f)
		}
	}
}

func TestResourceReadReadAndInvalidAccess(t *testing.T) {
	p := sample()
	for _, i := range []int{1, 2} {
		p.tasks[i].Effects = &project.Effects{Resources: []project.ResourceUse{{Name: "database", Access: project.ResourceRead}}}
	}
	r, err := Build(p, Request{Files: []string{"schema.json"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Conflicts) != 0 {
		t.Fatalf("read-only resource use conflicts: %+v", r.Conflicts)
	}
	p.tasks[1].Effects.Resources[0].Access = "unknown"
	r, err = Build(p, Request{Files: []string{"schema.json"}})
	if err != nil {
		t.Fatal(err)
	}
	if !issue(r, "invalid_resource") || r.Resolved {
		t.Fatalf("invalid resource declaration accepted: %+v", r)
	}
}
