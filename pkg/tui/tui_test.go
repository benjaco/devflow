package tui

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
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

func TestDownstreamInvalidateTasksForGroupReturnsItsCacheableInputs(t *testing.T) {
	g, err := graph.New([]project.Task{
		{Name: "build_a", Kind: project.KindOnce, Cache: true},
		{Name: "build_b", Kind: project.KindOnce, Cache: true},
		{Name: "aggregate", Kind: project.KindGroup, Deps: []string{"build_a", "build_b"}},
		{Name: "serve", Kind: project.KindService, Deps: []string{"aggregate"}},
	}, []project.Target{
		{Name: "main", RootTasks: []string{"serve"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	names, err := downstreamInvalidateTasks(g, "main", "aggregate")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names, ",")
	want := "build_a,build_b"
	if got != want {
		t.Fatalf("unexpected invalidate tasks for group: got %q want %q", got, want)
	}
}

func TestExecutionGraphResolvesTaskTargets(t *testing.T) {
	const name = "tui-execution-graph"
	project.Register(testProject{
		name: name,
		tasks: []project.Task{
			{Name: "build", Kind: project.KindOnce, Cache: true},
			{Name: "serve", Kind: project.KindService, Deps: []string{"build"}},
		},
		targets: []project.Target{
			{Name: "fullstack", RootTasks: []string{"serve"}},
		},
	})
	g, target, err := executionGraph(name, "build")
	if err != nil {
		t.Fatal(err)
	}
	if target != "build" {
		t.Fatalf("expected resolved synthetic target to be build, got %q", target)
	}
	closure, err := g.TargetClosure(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(closure, ","); got != "build" {
		t.Fatalf("unexpected synthetic target closure: %q", got)
	}
}

func TestResolveRelaunchProjectFallsBackToDetectedProject(t *testing.T) {
	worktree := t.TempDir()
	const marker = "detected.txt"
	if err := os.WriteFile(filepath.Join(worktree, marker), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	const name = "tui-relaunch-detected-project"
	project.Register(detectorTestProject{
		testProject: testProject{
			name: name,
			tasks: []project.Task{
				{Name: "build", Kind: project.KindOnce, Cache: true},
			},
			targets: []project.Target{
				{Name: "up", RootTasks: []string{"build"}},
			},
		},
		marker: marker,
	})

	inst := &api.Instance{}
	inst.LastRun.Project = "stale-project-name"

	gotName, gotProject, err := resolveRelaunchProject(worktree, inst)
	if err != nil {
		t.Fatal(err)
	}
	if gotName != name {
		t.Fatalf("unexpected project name: got %q want %q", gotName, name)
	}
	if gotProject.Name() != name {
		t.Fatalf("unexpected project: got %q want %q", gotProject.Name(), name)
	}
}

func TestWriteInvalidateTransitionMarksDirtyAndPendingNodes(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]api.NodeStatus{
		"build_a":   {Name: "build_a", Kind: "once", State: api.StateCached, LastRunKey: "a"},
		"build_b":   {Name: "build_b", Kind: "once", State: api.StateCached, LastRunKey: "b"},
		"aggregate": {Name: "aggregate", Kind: "group", State: api.StateDone},
		"serve":     {Name: "serve", Kind: "service", State: api.StateRunning, PID: 123},
	}
	if err := instance.SaveStatus(worktree, inst.ID, "main", api.ModeDev, nodes); err != nil {
		t.Fatal(err)
	}
	g, err := graph.New([]project.Task{
		{Name: "build_a", Kind: project.KindOnce, Cache: true},
		{Name: "build_b", Kind: project.KindOnce, Cache: true},
		{Name: "aggregate", Kind: project.KindGroup, Deps: []string{"build_a", "build_b"}},
		{Name: "serve", Kind: project.KindService, Deps: []string{"aggregate"}},
	}, []project.Target{{Name: "main", RootTasks: []string{"serve"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeInvalidateTransition(worktree, inst.ID, "main", g, []string{"build_a", "build_b"}); err != nil {
		t.Fatal(err)
	}
	state, err := instance.LoadStatus(worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Nodes["build_a"].State != api.StateDirty || state.Nodes["build_a"].LastRunKey != "" {
		t.Fatalf("expected build_a to become dirty without key, got %+v", state.Nodes["build_a"])
	}
	if state.Nodes["aggregate"].State != api.StatePending {
		t.Fatalf("expected aggregate to become pending, got %+v", state.Nodes["aggregate"])
	}
	if state.Nodes["serve"].State != api.StatePending || state.Nodes["serve"].PID != 0 {
		t.Fatalf("expected serve to become pending without pid, got %+v", state.Nodes["serve"])
	}
}

func TestRenderFooterIncludesRetargetKey(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	d.setStatus("[green]ready")
	text := d.footer.GetText(false)
	if !strings.Contains(text, "t retarget to selected task") {
		t.Fatalf("expected footer to advertise retarget key, got %q", text)
	}
}

func TestScrollLogsClampsAtTop(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	d.logs.SetText(strings.Repeat("line\n", 50))
	d.logs.ScrollTo(10, 0)
	d.scrollLogs(-100)
	row, _ := d.logs.GetScrollOffset()
	if row != 0 {
		t.Fatalf("expected log scroll to clamp at top, got row %d", row)
	}
}

func TestLoadSnapshotAllowsMissingInitialStatus(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	inst.LastRun.Target = "up"
	inst.LastRun.Mode = api.ModeDev
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}

	snap, err := loadSnapshot(worktree, inst.ID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if snap.state == nil {
		t.Fatal("expected placeholder state")
	}
	if snap.state.Target != "up" {
		t.Fatalf("unexpected placeholder target %q", snap.state.Target)
	}
	if snap.state.Mode != api.ModeDev {
		t.Fatalf("unexpected placeholder mode %q", snap.state.Mode)
	}
	if len(snap.nodes) != 0 {
		t.Fatalf("expected no nodes before initial status, got %d", len(snap.nodes))
	}
}

type testProject struct {
	name    string
	tasks   []project.Task
	targets []project.Target
}

func (p testProject) Name() string              { return p.name }
func (p testProject) Tasks() []project.Task     { return p.tasks }
func (p testProject) Targets() []project.Target { return p.targets }
func (p testProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	return project.InstanceConfig{}, nil
}

type detectorTestProject struct {
	testProject
	marker string
}

func (p detectorTestProject) DetectWorktree(worktree string) bool {
	_, err := os.Stat(filepath.Join(worktree, p.marker))
	return err == nil
}
