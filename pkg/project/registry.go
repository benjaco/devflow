package project

import (
	"fmt"
	"sort"
	"sync"
)

var (
	registryMu sync.RWMutex
	registry   = map[string]Project{}
)

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
