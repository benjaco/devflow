package project

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
)

var (
	registryMu sync.RWMutex
	registry   = map[string]Project{}
)

type WorktreeDetector interface {
	DetectWorktree(worktree string) bool
}

type DefaultTargeter interface {
	DefaultTarget() string
}

type syntheticTargetProject struct {
	base   Project
	target Target
}

func Register(p Project) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[p.Name()] = p
}

func Must(name string) Project {
	p, err := Lookup(name)
	if err != nil {
		panic(err)
	}
	return p
}

func Lookup(name string) (Project, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown project %q", name)
	}
	return p, nil
}

func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func Detect(worktree string) (Project, error) {
	worktree = filepath.Clean(worktree)
	names := Names()
	matches := make([]Project, 0, 1)
	for _, name := range names {
		p := registry[name]
		detector, ok := p.(WorktreeDetector)
		if !ok {
			continue
		}
		if detector.DetectWorktree(worktree) {
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("unable to detect project for worktree %q", worktree)
	default:
		names := make([]string, 0, len(matches))
		for _, p := range matches {
			names = append(names, p.Name())
		}
		sort.Strings(names)
		return nil, fmt.Errorf("ambiguous project detection for %q: %s", worktree, stringsJoin(names, ", "))
	}
}

func PreferredTarget(p Project) string {
	if targeter, ok := p.(DefaultTargeter); ok {
		if target := targeter.DefaultTarget(); target != "" {
			return target
		}
	}
	targets := p.Targets()
	priority := []string{"up", "fullstack", "frontend-stack", "services", "dev", "default"}
	for _, want := range priority {
		for _, target := range targets {
			if target.Name == want {
				return target.Name
			}
		}
	}
	if len(targets) == 1 {
		return targets[0].Name
	}
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.Name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func ResolveExecutionProject(p Project, target string) (Project, string, error) {
	if target == "" {
		return nil, "", fmt.Errorf("missing target")
	}
	for _, item := range p.Targets() {
		if item.Name == target {
			return p, target, nil
		}
	}
	for _, task := range p.Tasks() {
		if task.Name == target {
			return syntheticTargetProject{
				base:   p,
				target: Target{Name: target, RootTasks: []string{target}},
			}, target, nil
		}
	}
	return nil, "", fmt.Errorf("unknown target or task %q", target)
}

func (p syntheticTargetProject) Name() string  { return p.base.Name() }
func (p syntheticTargetProject) Tasks() []Task { return p.base.Tasks() }
func (p syntheticTargetProject) ConfigureInstance(ctx context.Context, worktree string) (InstanceConfig, error) {
	return p.base.ConfigureInstance(ctx, worktree)
}
func (p syntheticTargetProject) Targets() []Target {
	targets := append([]Target(nil), p.base.Targets()...)
	targets = append(targets, p.target)
	return targets
}

func stringsJoin(items []string, sep string) string {
	if len(items) == 0 {
		return ""
	}
	out := items[0]
	for _, item := range items[1:] {
		out += sep + item
	}
	return out
}
