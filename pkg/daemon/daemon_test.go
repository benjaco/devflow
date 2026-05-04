package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func TestPublishPersistsAndFansOutEvents(t *testing.T) {
	worktree := t.TempDir()
	instanceID, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		worktree:    worktree,
		instanceID:  instanceID,
		subscribers: map[chan api.Event]bool{},
	}
	ch := s.addSubscriber()
	defer s.removeSubscriber(ch)

	first := api.Event{Type: api.EventRunStarted, InstanceID: instanceID, Target: "fullstack"}
	second := api.Event{
		Type:          api.EventWatchCycleStart,
		InstanceID:    instanceID,
		Files:         []string{"frontend/src/page.tsx"},
		AffectedTasks: []string{"frontend_dev"},
	}
	s.publish(first)
	s.publish(second)

	for _, want := range []api.Event{first, second} {
		select {
		case got := <-ch:
			if got.Type != want.Type {
				t.Fatalf("unexpected event type %q, want %q", got.Type, want.Type)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for fanout event")
		}
	}

	data, err := os.ReadFile(instance.EventsPath(worktree, instanceID))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected 2 persisted event lines, got %d", len(lines))
	}
	var payload map[string]any
	if err := json.Unmarshal(lines[1], &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload["type"]; got != string(api.EventWatchCycleStart) {
		t.Fatalf("unexpected event type %v", got)
	}
	affected, ok := payload["affectedTasks"].([]any)
	if !ok || len(affected) != 1 || affected[0] != "frontend_dev" {
		t.Fatalf("unexpected affectedTasks payload: %v", payload["affectedTasks"])
	}
}

func TestEnsureSerializesDaemonStartup(t *testing.T) {
	worktree := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var starts atomic.Int32
	restore := SetStartDaemonFuncForTest(func(worktree, instanceID, projectName string) error {
		if starts.Add(1) > 1 {
			return nil
		}
		go func() {
			_ = Serve(ctx, Options{
				Worktree: worktree,
				Project:  projectName,
				LogPath:  filepath.Join(worktree, ".devflow", "logs", instanceID, "daemon.log"),
			})
		}()
		return nil
	})
	defer restore()

	const callers = 6
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, _, err := Ensure(callCtx, worktree, "")
			if err == nil {
				err = client.Ping(callCtx)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("expected one daemon start, got %d", got)
	}
}

func TestStartActiveReusesExistingMatchingWatch(t *testing.T) {
	name := "daemon-idempotent-watch"
	project.Register(daemonTestProject{
		name:    name,
		tasks:   []project.Task{{Name: "serve", Kind: project.KindService}},
		targets: []project.Target{{Name: "up", RootTasks: []string{"serve"}}},
	})
	active := &activeRun{
		projectName: name,
		target:      "up",
		mode:        api.ModeWatch,
		done:        make(chan struct{}),
	}
	s := &Server{
		worktree:   t.TempDir(),
		instanceID: "abc123",
		logPath:    filepath.Join(t.TempDir(), "daemon.log"),
		active:     active,
	}
	started, err := s.startActive(context.Background(), name, "up", api.ModeWatch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if started.Target != "up" || started.Mode != api.ModeWatch {
		t.Fatalf("unexpected start result: %+v", started)
	}
	if s.active != active {
		t.Fatal("expected matching active watch to be reused")
	}
}

func TestResolvePrismaMigrationTargetUsesComponentTaskFallback(t *testing.T) {
	p := daemonTestProject{
		name: "daemon-prisma-task-fallback",
		tasks: []project.Task{
			{Name: "custom_new_migration", Kind: project.KindOnce},
		},
	}
	target, err := resolvePrismaMigrationTarget(p)
	if err != nil {
		t.Fatal(err)
	}
	if target != "custom_new_migration" {
		t.Fatalf("unexpected fallback target %q", target)
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

func TestExecutionGraphForProjectResolvesTaskTargets(t *testing.T) {
	p := daemonTestProject{
		name: "daemon-execution-graph",
		tasks: []project.Task{
			{Name: "build", Kind: project.KindOnce, Cache: true},
			{Name: "serve", Kind: project.KindService, Deps: []string{"build"}},
		},
		targets: []project.Target{
			{Name: "fullstack", RootTasks: []string{"serve"}},
		},
	}
	g, target, err := executionGraphForProject(p, "build")
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

type daemonTestProject struct {
	name    string
	tasks   []project.Task
	targets []project.Target
}

func (p daemonTestProject) Name() string { return p.name }
func (p daemonTestProject) Tasks() []project.Task {
	return append([]project.Task(nil), p.tasks...)
}
func (p daemonTestProject) Targets() []project.Target {
	return append([]project.Target(nil), p.targets...)
}
func (p daemonTestProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: p.name}, nil
}
