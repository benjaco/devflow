package planner

import (
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/project"
)

type resourceAccess struct {
	name  string
	write bool
	file  bool
}

func resourceConflicts(g *graph.Graph, closure []string) []ResourceConflict {
	conflicts := []ResourceConflict{}
	for i, a := range closure {
		for _, b := range closure[i+1:] {
			// Dependencies already serialize resource use inside a single DAG.
			if slices.Contains(g.Upstream([]string{b}), a) || slices.Contains(g.Upstream([]string{a}), b) {
				continue
			}
			seen := map[string]bool{}
			for _, left := range taskResources(g.Tasks[a]) {
				for _, right := range taskResources(g.Tasks[b]) {
					if left.file != right.file || (!left.write && !right.write) {
						continue
					}
					resource := left.name
					if left.file {
						if !pathsOverlap(left.name, right.name) {
							continue
						}
						if len(right.name) < len(resource) {
							resource = right.name
						}
						resource = "file:" + resource
					} else if left.name != right.name {
						continue
					}
					if seen[resource] {
						continue
					}
					seen[resource] = true
					conflicts = append(conflicts, ResourceConflict{Resource: resource, Tasks: unique([]string{a, b}), Reason: "unordered tasks declare overlapping access, including a write"})
				}
			}
		}
	}
	sort.Slice(conflicts, func(i, j int) bool {
		a, b := conflicts[i], conflicts[j]
		if a.Resource != b.Resource {
			return a.Resource < b.Resource
		}
		return strings.Join(a.Tasks, "\x00") < strings.Join(b.Tasks, "\x00")
	})
	return conflicts
}

func taskResources(task project.Task) []resourceAccess {
	out := []resourceAccess{}
	reads := append([]string{}, task.Inputs.Paths...)
	reads = append(reads, task.Inputs.Files...)
	reads = append(reads, task.Inputs.Dirs...)
	reads = append(reads, task.Inputs.Globs...)
	for _, input := range task.Inputs.Filtered {
		reads = append(reads, input.Path)
	}
	for _, file := range unique(reads) {
		out = append(out, resourceAccess{name: writePrefix(file), file: true})
	}
	writes := append([]string{}, task.Outputs.Paths...)
	writes = append(writes, task.Outputs.Files...)
	writes = append(writes, task.Outputs.Dirs...)
	if task.Effects != nil {
		writes = append(writes, task.Effects.Writes...)
		for _, name := range task.Effects.Touches {
			out = append(out, resourceAccess{name: name, write: true})
		}
		for _, use := range task.Effects.Resources {
			out = append(out, resourceAccess{name: use.Name, write: use.Access != project.ResourceRead})
		}
	}
	for _, file := range unique(writes) {
		out = append(out, resourceAccess{name: writePrefix(file), write: true, file: true})
	}
	return out
}

func writePrefix(value string) string {
	value = filepath.ToSlash(value)
	// Globs describe possible writes, not observed artifacts. Their literal
	// directory prefix deliberately overestimates overlap without scanning files.
	if i := strings.IndexAny(value, "*?["); i >= 0 {
		prefix := value[:i]
		if j := strings.LastIndex(prefix, "/"); j >= 0 {
			return path.Clean(prefix[:j])
		}
		return "."
	}
	return path.Clean(value)
}
func pathsOverlap(a, b string) bool {
	return a == "." || b == "." || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
