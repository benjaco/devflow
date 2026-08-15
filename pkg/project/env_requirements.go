package project

import (
	"fmt"
	"sort"
	"strings"
)

type RequiredEnvProvider interface {
	RequiredEnvs() []string
}

func RequiredEnvsFor(p Project) []string {
	selected := map[string]bool{}
	if provider, ok := p.(RequiredEnvProvider); ok {
		addRequiredEnv(selected, provider.RequiredEnvs()...)
	}
	for _, task := range p.Tasks() {
		addRequiredEnv(selected, task.RequiredEnv...)
	}
	for _, target := range p.Targets() {
		addRequiredEnv(selected, target.RequiredEnv...)
	}
	return sortedRequiredEnv(selected)
}

func RequiredEnvsForTarget(p Project, target string) ([]string, error) {
	targetDef, ok := targetByName(p.Targets(), target)
	if !ok {
		return nil, fmt.Errorf("unknown target %q", target)
	}
	selected := map[string]bool{}
	if provider, ok := p.(RequiredEnvProvider); ok {
		addRequiredEnv(selected, provider.RequiredEnvs()...)
	}
	addRequiredEnv(selected, targetDef.RequiredEnv...)
	tasks := map[string]Task{}
	for _, task := range p.Tasks() {
		tasks[task.Name] = task
	}
	state := map[string]int{}
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("cycle detected at task %q", name)
		case 2:
			return nil
		}
		task, ok := tasks[name]
		if !ok {
			return fmt.Errorf("target %q references missing task %q", targetDef.Name, name)
		}
		state[name] = 1
		addRequiredEnv(selected, task.RequiredEnv...)
		for _, dep := range task.Deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[name] = 2
		return nil
	}
	for _, root := range targetDef.RootTasks {
		if err := visit(root); err != nil {
			return nil, err
		}
	}
	return sortedRequiredEnv(selected), nil
}

func addRequiredEnv(selected map[string]bool, keys ...string) {
	for _, key := range keys {
		if key = strings.TrimSpace(key); key != "" {
			selected[key] = true
		}
	}
}

func sortedRequiredEnv(selected map[string]bool) []string {
	keys := make([]string, 0, len(selected))
	for key := range selected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
