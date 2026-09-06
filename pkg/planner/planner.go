// Package planner selects verification work from declarations without executing it.
package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/benjaco/devflow/internal/adaptersource"
	"github.com/benjaco/devflow/internal/clierror"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/project"
)

type Request struct {
	Files  []string
	Intent string
}

type Result struct {
	Project              string               `json:"project"`
	Intent               string               `json:"intent"`
	Advisory             bool                 `json:"advisory"`
	Resolved             bool                 `json:"resolved"`
	Files                []string             `json:"files"`
	GraphDigest          string               `json:"graphDigest"`
	ScopeDigest          string               `json:"scopeDigest"`
	ConfigurationChanged bool                 `json:"configurationChanged"`
	FileImpacts          []FileImpact         `json:"fileImpacts"`
	Checks               []Check              `json:"checks"`
	Closure              []string             `json:"closure"`
	Tasks                []graph.TaskMetadata `json:"tasks"`
	SharedDependencies   []SharedDependency   `json:"sharedDependencies"`
	Prerequisites        Prerequisites        `json:"prerequisites"`
	Conflicts            []ResourceConflict   `json:"conflicts"`
	Issues               []Issue              `json:"issues"`
}

type FileImpact struct {
	File          string   `json:"file"`
	State         string   `json:"state"`
	DirectTasks   []string `json:"directTasks"`
	AffectedTasks []string `json:"affectedTasks"`
	Checks        []string `json:"checks"`
}

type Reason struct {
	File string `json:"file"`
	Task string `json:"task,omitempty"`
	Kind string `json:"kind"`
}

type Check struct {
	Name              string   `json:"name"`
	Kind              string   `json:"kind"`
	Reasons           []Reason `json:"reasons"`
	Closure           []string `json:"closure"`
	Command           []string `json:"command"`
	ValidationCommand []string `json:"validationCommand,omitempty"`
}

type SharedDependency struct {
	Task   string   `json:"task"`
	Checks []string `json:"checks"`
}

type Prerequisites struct {
	CLIs         []string `json:"clis"`
	Env          []string `json:"env"`
	Availability string   `json:"availability"`
}

type ResourceConflict struct {
	Resource string   `json:"resource"`
	Tasks    []string `json:"tasks"`
	Reason   string   `json:"reason"`
}

type Issue struct {
	Code    string `json:"code"`
	File    string `json:"file,omitempty"`
	Task    string `json:"task,omitempty"`
	Message string `json:"message"`
}

func Build(p project.Project, req Request) (*Result, error) {
	if req.Intent == "" {
		req.Intent = "verify"
	}
	if req.Intent != "verify" {
		return nil, clierror.Wrap(fmt.Errorf("unsupported intent %q; expected verify", req.Intent), "invalid_arguments", "parsing")
	}
	files, err := normalizeFiles(req.Files)
	if err != nil {
		return nil, clierror.Wrap(err, "invalid_arguments", "parsing")
	}
	g, err := graph.New(p.Tasks(), p.Targets())
	if err != nil {
		return nil, err
	}
	metadata := g.Metadata()
	r := &Result{Project: p.Name(), Intent: req.Intent, Advisory: true, Files: files, ScopeDigest: digest(files),
		FileImpacts: []FileImpact{}, Checks: []Check{}, Closure: []string{}, Tasks: []graph.TaskMetadata{}, SharedDependencies: []SharedDependency{},
		Prerequisites: Prerequisites{CLIs: []string{}, Env: []string{}, Availability: "unchecked"}, Conflicts: []ResourceConflict{}, Issues: []Issue{}}
	// Only declaration fields enter planning identity; commands and env values
	// may contain credentials. CLI source identity separately captures adapter code.
	type actionMetadata struct {
		Task     string
		Category project.ActionCategory
		Effects  project.Effects
	}
	actions := []actionMetadata{}
	actionTasks := map[string]bool{}
	for _, a := range project.Actions(p) {
		actions = append(actions, actionMetadata{a.Task, a.Category, a.Effects})
		actionTasks[a.Task] = true
	}
	globalEnv := []string{}
	if provider, ok := p.(project.RequiredEnvProvider); ok {
		globalEnv = unique(provider.RequiredEnvs())
	}
	catalog := []string{}
	for _, cli := range project.RequiredCLIsFor(p) {
		catalog = append(catalog, cli.Name)
	}
	r.GraphDigest = digest(struct {
		Metadata  graph.Metadata
		Actions   []actionMetadata
		CLIs, Env []string
	}{metadata, actions, unique(catalog), globalEnv})
	selected := map[string]*Check{}
	add := func(name, kind, file string, direct []string) bool {
		var closure []string
		if kind == "target" {
			closure, err = g.TargetClosure(name)
		} else {
			closure, err = g.TopoSort(g.Upstream([]string{name}))
			if target, ok := g.Targets[name]; ok && !(len(target.RootTasks) == 1 && target.RootTasks[0] == name) {
				r.Issues = append(r.Issues, Issue{Code: "ambiguous_execution_name", File: file, Task: name, Message: "a target shadows this task in devflow run"})
				return false
			}
		}
		if err != nil {
			return false
		} // Graph validation already established these closures.
		for _, task := range closure {
			def := g.Tasks[task]
			if project.IsServiceKind(def.Kind) || actionTasks[task] || slices.Contains(def.Purposes, project.PurposeFormat) || (def.Effects != nil && len(def.Effects.Invalidates) > 0) {
				r.Issues = append(r.Issues, Issue{Code: "unsafe_verification", File: file, Task: task, Message: "verification would execute a service, action, formatter or invalidation; declare a finite verification target"})
				return false
			}
		}
		key := kind + ":" + name
		c := selected[key]
		if c == nil {
			c = &Check{Name: name, Kind: kind, Closure: closure, Reasons: []Reason{}, Command: []string{"devflow", "run", name, "--ci", "--json", "--project", p.Name()}}
			selected[key] = c
		}
		if adaptersource.IsSource(file) {
			c.Reasons = append(c.Reasons, Reason{File: file, Kind: "configuration_changed"})
			c.ValidationCommand = []string{"devflow", "validate", name, "--mode", "artifacts", "--json", "--project", p.Name()}
		} else {
			for _, task := range direct {
				if slices.Contains(closure, task) {
					kind := "downstream"
					if task == name {
						kind = "direct_input"
					}
					c.Reasons = append(c.Reasons, Reason{File: file, Task: task, Kind: kind})
				}
			}
		}
		return true
	}
	for _, file := range files {
		f := FileImpact{File: file, State: "unmatched", DirectTasks: []string{}, AffectedTasks: []string{}, Checks: []string{}}
		if adaptersource.IsSource(file) {
			r.ConfigurationChanged = true
			f.State = "configuration"
			for _, target := range metadata.Targets {
				if target.Verification && add(target.Name, "target", file, nil) {
					f.Checks = append(f.Checks, target.Name)
				}
			}
			if len(f.Checks) == 0 {
				r.Issues = append(r.Issues, Issue{Code: "configuration_impact_unknown", File: file, Message: "adapter changes require an explicit full verification target; input matching cannot establish their impact"})
			}
		} else {
			impacts := g.ExplainAffectedByFiles([]string{file})
			for _, impact := range impacts {
				if impact.Affected {
					f.DirectTasks = append(f.DirectTasks, impact.Task)
				} else if f.State == "unmatched" {
					f.State = "ignored"
				}
			}
			if len(f.DirectTasks) > 0 {
				f.State = "matched"
				f.AffectedTasks = g.Downstream(f.DirectTasks)
				for _, name := range f.AffectedTasks {
					if verificationPurpose(g.Tasks[name].Purposes) && add(name, "task", file, f.DirectTasks) {
						f.Checks = append(f.Checks, name)
					}
				}
				uncovered := func() []string {
					missing := []string{}
					for _, name := range f.DirectTasks {
						def := g.Tasks[name]
						if project.IsServiceKind(def.Kind) || actionTasks[name] || slices.Contains(def.Purposes, project.PurposeFormat) {
							continue
						}
						covered := false
						for _, check := range selected {
							if slices.Contains(check.Closure, name) {
								covered = true
								break
							}
						}
						if !covered {
							missing = append(missing, name)
						}
					}
					return missing
				}
				// A check for one consumer does not cover another consumer of the
				// same file. Resolve each eligible input branch independently.
				for _, target := range metadata.Targets {
					if !target.Verification {
						continue
					}
					closure, _ := g.TargetClosure(target.Name)
					alreadySelected := selected["target:"+target.Name] != nil && intersects(closure, f.DirectTasks)
					if (alreadySelected || intersects(closure, uncovered())) && add(target.Name, "target", file, f.DirectTasks) {
						f.Checks = append(f.Checks, target.Name)
					}
				}
				for _, name := range uncovered() {
					r.Issues = append(r.Issues, Issue{Code: "uncovered_impact", File: file, Task: name, Message: "this affected input branch has no declared verification check"})
				}
				if len(f.Checks) == 0 {
					r.Issues = append(r.Issues, Issue{Code: "no_verification_check", File: file, Message: "affected tasks have no eligible verification purpose or finite verification target"})
				}
			} else if f.State == "unmatched" {
				r.Issues = append(r.Issues, Issue{Code: "unmatched_file", File: file, Message: "no declared task input matches this file; coverage is unknown"})
			}
		}
		f.Checks = unique(f.Checks)
		r.FileImpacts = append(r.FileImpacts, f)
	}
	// A selected target already executes its constituent checks. Retain one
	// recommendation and merge reasons instead of suggesting duplicate commands.
	for key, c := range selected {
		if c.Kind != "task" {
			continue
		}
		covered := false
		for _, target := range selected {
			if target.Kind == "target" && slices.Contains(target.Closure, c.Name) {
				target.Reasons = append(target.Reasons, c.Reasons...)
				covered = true
			}
		}
		if covered {
			delete(selected, key)
		}
	}
	for _, c := range selected {
		c.Reasons = uniqueReasons(c.Reasons)
		r.Checks = append(r.Checks, *c)
	}
	sort.Slice(r.Checks, func(i, j int) bool {
		if r.Checks[i].Name != r.Checks[j].Name {
			return r.Checks[i].Name < r.Checks[j].Name
		}
		return r.Checks[i].Kind < r.Checks[j].Kind
	})
	used := map[string][]string{}
	cliNames, envNames := []string{}, []string{}
	for _, c := range r.Checks {
		for _, task := range c.Closure {
			used[task] = append(used[task], c.Name)
		}
		executionProject, target, e := project.ResolveExecutionProject(p, c.Name)
		if e != nil {
			return nil, e
		}
		clis, e := project.RequiredCLIsForTarget(executionProject, target)
		if e != nil {
			return nil, clierror.Wrap(e, "invalid_prerequisite", "resolution")
		}
		envs, e := project.RequiredEnvsForTarget(executionProject, target)
		if e != nil {
			return nil, e
		}
		for _, cli := range clis {
			cliNames = append(cliNames, cli.Name)
		}
		envNames = append(envNames, envs...)
	}
	for task, checks := range used {
		r.Closure = append(r.Closure, task)
		if len(checks) > 1 {
			r.SharedDependencies = append(r.SharedDependencies, SharedDependency{Task: task, Checks: unique(checks)})
		}
	}
	r.Closure, _ = g.TopoSort(r.Closure)
	sort.Slice(r.SharedDependencies, func(i, j int) bool { return r.SharedDependencies[i].Task < r.SharedDependencies[j].Task })
	r.Prerequisites.CLIs = unique(cliNames)
	r.Prerequisites.Env = unique(envNames)
	for _, task := range metadata.Tasks {
		if task.Inputs.CustomFingerprintCount > 0 || task.HasCacheKeyOverride {
			r.Issues = append(r.Issues, Issue{Code: "opaque_inputs", Task: task.Name, Message: "custom input or cache-key callbacks are not evaluated; declared paths may not describe all inputs"})
		}
		if _, ok := used[task.Name]; !ok {
			continue
		}
		r.Tasks = append(r.Tasks, task)
		if task.Kind != project.KindGroup && task.Effects == nil {
			r.Issues = append(r.Issues, Issue{Code: "unknown_effects", Task: task.Name, Message: "task effects are undeclared; artifact outputs do not describe every side effect"})
		}
		if task.Effects != nil {
			for _, use := range task.Effects.Resources {
				if strings.TrimSpace(use.Name) == "" || (use.Access != project.ResourceRead && use.Access != project.ResourceWrite) {
					r.Issues = append(r.Issues, Issue{Code: "invalid_resource", Task: task.Name, Message: "resources require a nonempty name and read or write access"})
				}
			}
		}
	}
	// Reconcile per-file references after target deduplication.
	for i := range r.FileImpacts {
		f := &r.FileImpacts[i]
		f.Checks = []string{}
		for _, c := range r.Checks {
			for _, reason := range c.Reasons {
				if reason.File == f.File {
					f.Checks = append(f.Checks, c.Name)
					break
				}
			}
		}
	}
	r.Conflicts = resourceConflicts(g, r.Closure)
	r.Issues = uniqueIssues(r.Issues)
	r.Resolved = len(r.Issues) == 0 && len(r.Conflicts) == 0
	return r, nil
}

func normalizeFiles(files []string) ([]string, error) {
	out := []string{}
	for _, file := range files {
		file = filepath.ToSlash(strings.TrimSpace(file))
		// Windows IsAbs returns false for /path, but it is still rooted outside the worktree.
		if strings.HasPrefix(file, "/") || filepath.VolumeName(file) != "" || (len(file) > 1 && file[1] == ':') {
			return nil, fmt.Errorf("file must be worktree-relative: %q", file)
		}
		for _, part := range strings.Split(file, "/") {
			if part == ".." {
				return nil, fmt.Errorf("file must not traverse parents: %q", file)
			}
		}
		file = filepath.ToSlash(filepath.Clean(file))
		if file == "." || file == "" {
			return nil, fmt.Errorf("expected a changed file path")
		}
		out = append(out, file)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("provide at least one changed file")
	}
	return unique(out), nil
}
func verificationPurpose(purposes []project.Purpose) bool {
	for _, p := range purposes {
		switch p {
		case project.PurposeTest, project.PurposeLint, project.PurposeTypecheck, project.PurposeBuild:
			return true
		}
	}
	return false
}
func unique(values []string) []string {
	out := append([]string{}, values...)
	sort.Strings(out)
	return slices.Compact(out)
}
func intersects(a, b []string) bool {
	for _, v := range a {
		if slices.Contains(b, v) {
			return true
		}
	}
	return false
}
func digest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func uniqueReasons(values []Reason) []Reason {
	sort.Slice(values, func(i, j int) bool {
		a, b := values[i], values[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Task != b.Task {
			return a.Task < b.Task
		}
		return a.Kind < b.Kind
	})
	return slices.Compact(values)
}
func uniqueIssues(values []Issue) []Issue {
	sort.Slice(values, func(i, j int) bool {
		a, b := values[i], values[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Task != b.Task {
			return a.Task < b.Task
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Message < b.Message
	})
	return slices.Compact(values)
}
