package graph

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"

	"github.com/benjaco/devflow/pkg/project"
)

type Graph struct {
	Tasks   map[string]project.Task
	Targets map[string]project.Target
}

func New(tasks []project.Task, targets []project.Target) (*Graph, error) {
	g := &Graph{
		Tasks:   make(map[string]project.Task, len(tasks)),
		Targets: make(map[string]project.Target, len(targets)),
	}

	for _, task := range tasks {
		if _, ok := g.Tasks[task.Name]; ok {
			return nil, fmt.Errorf("duplicate task %q", task.Name)
		}
		g.Tasks[task.Name] = task
	}
	for _, target := range targets {
		if _, ok := g.Targets[target.Name]; ok {
			return nil, fmt.Errorf("duplicate target %q", target.Name)
		}
		g.Targets[target.Name] = target
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *Graph) Validate() error {
	for _, task := range g.Tasks {
		if task.Kind == project.KindGroup && task.Run != nil {
			return fmt.Errorf("group task %q cannot define Run", task.Name)
		}
		if task.Cache && task.Kind != project.KindOnce {
			return fmt.Errorf("only once tasks may be cacheable: %q", task.Name)
		}
		for _, dep := range task.Deps {
			if _, ok := g.Tasks[dep]; !ok {
				return fmt.Errorf("task %q references missing dependency %q", task.Name, dep)
			}
		}
	}
	for _, target := range g.Targets {
		for _, root := range target.RootTasks {
			if _, ok := g.Tasks[root]; !ok {
				return fmt.Errorf("target %q references missing task %q", target.Name, root)
			}
		}
	}

	visited := map[string]int{}
	var visit func(string) error
	visit = func(name string) error {
		switch visited[name] {
		case 1:
			return fmt.Errorf("cycle detected at task %q", name)
		case 2:
			return nil
		}
		visited[name] = 1
		for _, dep := range g.Tasks[name].Deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visited[name] = 2
		return nil
	}
	for name := range g.Tasks {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

func (g *Graph) TargetClosure(target string) ([]string, error) {
	t, ok := g.Targets[target]
	if !ok {
		return nil, fmt.Errorf("unknown target %q", target)
	}
	seen := map[string]bool{}
	var visit func(string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, dep := range g.Tasks[name].Deps {
			visit(dep)
		}
	}
	for _, root := range t.RootTasks {
		visit(root)
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	order, err := g.TopoSort(names)
	if err != nil {
		return nil, err
	}
	return order, nil
}

func (g *Graph) TopoSort(subset []string) ([]string, error) {
	allowed := map[string]bool{}
	for _, name := range subset {
		allowed[name] = true
	}
	visited := map[string]bool{}
	order := make([]string, 0, len(subset))
	var visit func(string)
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		for _, dep := range g.Tasks[name].Deps {
			if allowed[dep] {
				visit(dep)
			}
		}
		order = append(order, name)
	}
	sorted := append([]string(nil), subset...)
	sort.Strings(sorted)
	for _, name := range sorted {
		visit(name)
	}
	return order, nil
}

func (g *Graph) Downstream(names []string) []string {
	seen := map[string]bool{}
	queue := append([]string(nil), names...)
	reverse := map[string][]string{}
	for _, task := range g.Tasks {
		for _, dep := range task.Deps {
			reverse[dep] = append(reverse[dep], task.Name)
		}
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		queue = append(queue, reverse[name]...)
	}
	return sortedKeys(seen)
}

func (g *Graph) Upstream(names []string) []string {
	seen := map[string]bool{}
	var visit func(string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, dep := range g.Tasks[name].Deps {
			visit(dep)
		}
	}
	for _, name := range names {
		visit(name)
	}
	return sortedKeys(seen)
}

func (g *Graph) AffectedByFiles(files []string) []string {
	affected := map[string]bool{}
	for _, task := range g.Tasks {
		for _, changed := range files {
			if matchesTaskInput(task, filepath.Clean(changed)) {
				affected[task.Name] = true
				break
			}
		}
	}
	return sortedKeys(affected)
}

func matchesTaskInput(task project.Task, changed string) bool {
	for _, ignore := range task.Inputs.Ignore {
		if ok, _ := filepath.Match(ignore, changed); ok {
			return false
		}
	}
	for _, file := range task.Inputs.Files {
		if filepath.Clean(file) == changed {
			return true
		}
	}
	for _, dir := range task.Inputs.Dirs {
		dir = filepath.Clean(dir)
		if changed == dir || stringsHasPathPrefix(changed, dir) {
			return true
		}
	}
	return false
}

func stringsHasPathPrefix(changed, dir string) bool {
	changed = filepath.ToSlash(changed)
	dir = filepath.ToSlash(dir)
	if dir == "." || dir == "" {
		return true
	}
	if !path.IsAbs(changed) && !path.IsAbs(dir) && changed == dir {
		return true
	}
	return len(changed) > len(dir) && changed[:len(dir)] == dir && changed[len(dir)] == '/'
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
