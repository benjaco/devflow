package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"devflow/pkg/api"
	"devflow/pkg/graph"
	"devflow/pkg/instance"
	"devflow/pkg/project"
)

func TestReadLastLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.log")
	content := strings.Join([]string{"one", "two", "three", "four"}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := readLastLines(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lines, ","); got != "three,four" {
		t.Fatalf("unexpected tail lines: %s", got)
	}
}

func TestTaskStatePriorityOrdersRunningThenPending(t *testing.T) {
	nodes := []api.NodeStatus{
		{Name: "done_task", State: api.StateDone},
		{Name: "pending_task", State: api.StatePending},
		{Name: "running_task", State: api.StateRunning},
		{Name: "cached_task", State: api.StateCached},
	}
	sort.Slice(nodes, func(i, j int) bool {
		left := taskStatePriority(nodes[i].State)
		right := taskStatePriority(nodes[j].State)
		if left != right {
			return left < right
		}
		return nodes[i].Name < nodes[j].Name
	})
	got := []string{nodes[0].Name, nodes[1].Name, nodes[2].Name, nodes[3].Name}
	want := []string{"running_task", "pending_task", "cached_task", "done_task"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected node order: got %v want %v", got, want)
	}
}

func TestRenderHeaderIncludesStateSummary(t *testing.T) {
	snap := snapshot{
		instance: &api.Instance{
			ID:       "abc123",
			Worktree: "/tmp/worktree",
			DB: api.DBInstance{
				Name:          "coach",
				Host:          "127.0.0.1",
				Port:          5433,
				ContainerName: "devflow-pg-abc123",
			},
		},
		state: &instance.State{
			Target:    "fullstack",
			Mode:      api.ModeDev,
			UpdatedAt: time.Now().UTC(),
		},
		nodes: []api.NodeStatus{
			{Name: "postgres", State: api.StateRunning},
			{Name: "backend_dev", State: api.StateRunning},
			{Name: "build", State: api.StateCached},
			{Name: "done", State: api.StateDone},
		},
		supervisor: &api.SupervisorStatus{PID: 55, Alive: true},
		urls:       map[string]string{"backend": "http://127.0.0.1:8080"},
	}
	lines := renderHeader(snap)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"instance",
		"abc123",
		"backend=http://127.0.0.1:8080",
		"RUN=2",
		"CACHE=1",
		"DONE=1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected header to contain %q, got:\n%s", want, joined)
		}
	}
}

func TestRenderLogPanelIncludesSelection(t *testing.T) {
	snap := snapshot{
		nodes: []api.NodeStatus{
			{Name: "backend_dev", Kind: "service", State: api.StateRunning, PID: 12},
		},
		logTitle: "backend_dev log",
		logLines: []string{"line one", "line two"},
	}
	lines := renderLogPanel(snap, "backend_dev")
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"selected: backend_dev",
		"kind=service",
		"state=running",
		"line two",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected log panel to contain %q, got:\n%s", want, joined)
		}
	}
}

func TestDownstreamInvalidateTasksOnlyReturnsCacheableOnceTasksInTargetClosure(t *testing.T) {
	g, err := graph.New([]project.Task{
		{Name: "a", Kind: project.KindOnce, Cache: true},
		{Name: "b", Kind: project.KindOnce, Cache: true, Deps: []string{"a"}},
		{Name: "c", Kind: project.KindService, Deps: []string{"b"}},
		{Name: "d", Kind: project.KindOnce, Cache: false, Deps: []string{"b"}},
		{Name: "e", Kind: project.KindOnce, Cache: true, Deps: []string{"d"}},
	}, []project.Target{
		{Name: "main", RootTasks: []string{"c", "e"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	names, err := downstreamInvalidateTasks(g, "main", "b")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names, ",")
	want := "b,e"
	if got != want {
		t.Fatalf("unexpected invalidate tasks: got %q want %q", got, want)
	}
}
