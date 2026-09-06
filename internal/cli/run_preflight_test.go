package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

type outputlessCachedCLIProject struct {
	configured atomic.Bool
	ran        atomic.Bool
}

func (*outputlessCachedCLIProject) Name() string { return "cli-outputless-cached-project" }

func (p *outputlessCachedCLIProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	p.configured.Store(true)
	return project.InstanceConfig{}, nil
}

func (*outputlessCachedCLIProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"invalid"}}}
}

func (p *outputlessCachedCLIProject) Tasks() []project.Task {
	return []project.Task{{
		Name:  "invalid",
		Kind:  project.KindOnce,
		Cache: true,
		Run: func(_ context.Context, rt *project.Runtime) error {
			p.ran.Store(true)
			return os.WriteFile(filepath.Join(rt.Worktree, "executed.txt"), []byte("ran"), 0o644)
		},
	}}
}

func TestRunCIJSONReportsEnginePreflightFailureWithoutMutation(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("HOME", cacheRoot)
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("LOCALAPPDATA", cacheRoot)
	worktree := t.TempDir()
	p := &outputlessCachedCLIProject{}
	project.Register(p)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: stderr}
	runErr := app.Run([]string{"run", "build", "--ci", "--json", "--project", p.Name(), "--worktree", worktree})
	if runErr == nil || !strings.Contains(runErr.Error(), `cacheable task "invalid" must declare outputs`) {
		t.Fatalf("expected cached-output preflight error, got %v", runErr)
	}
	var result api.RunResult
	decoder := json.NewDecoder(stdout)
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("preflight failure did not emit a RunResult: %v; stderr=%s", err, stderr)
	}
	if result.Success || result.Error == nil || result.Error.Message != runErr.Error() || result.Target != "build" || result.Mode != api.ModeCI || result.InstanceID == "" {
		t.Fatalf("unexpected preflight failure JSON: %+v", result)
	}
	if result.RepositoryChanges != nil || len(result.Nodes) != 0 {
		t.Fatalf("preflight result claims repository repair or task execution: %+v", result)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("expected exactly one JSON result, got trailing value %v (error %v)", extra, err)
	}
	if p.configured.Load() || p.ran.Load() {
		t.Fatalf("invalid cached task reached instance configuration or execution: configured=%t ran=%t", p.configured.Load(), p.ran.Load())
	}
	entries, err := os.ReadDir(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("preflight failure mutated the worktree: %v", entries)
	}
}
