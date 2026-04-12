package ports

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"devflow/internal/jsonutil"
	"devflow/internal/lock"
	"devflow/pkg/instance"
)

type Registry struct {
	Instances map[string]map[string]int `json:"instances"`
}

type Manager struct {
	Path string
}

func NewDefault() (*Manager, error) {
	root, err := instance.GlobalStateRoot()
	if err != nil {
		return nil, err
	}
	return &Manager{Path: filepath.Join(root, "ports.json")}, nil
}

func (m *Manager) Allocate(instanceID string, names []string) (map[string]int, error) {
	lockFile, err := lock.Acquire(m.Path + ".lock")
	if err != nil {
		return nil, err
	}
	defer lockFile.Release()

	registry, err := m.load()
	if err != nil {
		return nil, err
	}
	if registry.Instances == nil {
		registry.Instances = map[string]map[string]int{}
	}

	current := registry.Instances[instanceID]
	if current == nil {
		current = map[string]int{}
	}

	used := map[int]bool{}
	for _, ports := range registry.Instances {
		for _, port := range ports {
			used[port] = true
		}
	}

	for _, name := range names {
		if port := current[name]; port != 0 && portAvailable(port) {
			continue
		}
		port, err := freePort(used)
		if err != nil {
			return nil, err
		}
		current[name] = port
		used[port] = true
	}
	registry.Instances[instanceID] = current
	if err := jsonutil.WriteFileAtomic(m.Path, registry); err != nil {
		return nil, err
	}
	return current, nil
}

func (m *Manager) Release(instanceID string) error {
	lockFile, err := lock.Acquire(m.Path + ".lock")
	if err != nil {
		return err
	}
	defer lockFile.Release()

	registry, err := m.load()
	if err != nil {
		return err
	}
	delete(registry.Instances, instanceID)
	return jsonutil.WriteFileAtomic(m.Path, registry)
}

func (m *Manager) load() (Registry, error) {
	registry := Registry{Instances: map[string]map[string]int{}}
	if err := jsonutil.ReadFile(m.Path, &registry); err != nil {
		if os.IsNotExist(err) {
			return registry, nil
		}
		return Registry{}, err
	}
	return registry, nil
}

func freePort(used map[int]bool) (int, error) {
	for i := 0; i < 32; i++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		if !used[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("unable to allocate free port")
}

func portAvailable(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}
