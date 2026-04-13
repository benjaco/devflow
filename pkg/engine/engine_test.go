package engine

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"devflow/pkg/api"
	"devflow/pkg/instance"
	"devflow/pkg/process"
	"devflow/pkg/project"
)

type testProject struct{}

func (testProject) Name() string { return "test-project" }

func (testProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "test"}, nil
}

func (testProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:      "gen",
			Kind:      project.KindOnce,
			Cache:     true,
			Inputs:    project.Inputs{Files: []string{"input.txt"}},
			Outputs:   project.Outputs{Files: []string{"out.txt"}},
			Signature: "gen-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				data, err := os.ReadFile(filepath.Join(rt.Worktree, "input.txt"))
				if err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(rt.Worktree, "out.txt"), data, 0o644)
			},
		},
	}
}

func (testProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"gen"}}}
}

func TestRunUsesCacheOnSecondExecution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, err := New(testProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	first, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Result.CacheHits) != 0 {
		t.Fatalf("unexpected cache hits on first run: %v", first.Result.CacheHits)
	}
	second, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Result.CacheHits) != 1 || second.Result.CacheHits[0] != "gen" {
		t.Fatalf("unexpected cache hits on second run: %v", second.Result.CacheHits)
	}
}

type parallelProject struct {
	maxSeen atomic.Int32
	current atomic.Int32
}

func (p *parallelProject) Name() string { return "parallel-project" }

func (p *parallelProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "parallel"}, nil
}

func (p *parallelProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"join"}}}
}

func (p *parallelProject) Tasks() []project.Task {
	makeTask := func(name string) project.Task {
		return project.Task{
			Name: name,
			Kind: project.KindOnce,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = rt
				current := p.current.Add(1)
				defer p.current.Add(-1)
				for {
					max := p.maxSeen.Load()
					if current <= max || p.maxSeen.CompareAndSwap(max, current) {
						break
					}
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(120 * time.Millisecond):
					return nil
				}
			},
		}
	}

	return []project.Task{
		makeTask("a"),
		makeTask("b"),
		{
			Name: "join",
			Kind: project.KindOnce,
			Deps: []string{"a", "b"},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				_ = rt
				return nil
			},
		},
	}
}

func TestRunParallelizesIndependentTasks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	p := &parallelProject{}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI, MaxParallel: 2}); err != nil {
		t.Fatal(err)
	}
	if got := p.maxSeen.Load(); got < 2 {
		t.Fatalf("expected parallel execution, max concurrent = %d", got)
	}
}

func TestRunHonorsMaxParallelOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	p := &parallelProject{}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI, MaxParallel: 1}); err != nil {
		t.Fatal(err)
	}
	if got := p.maxSeen.Load(); got != 1 {
		t.Fatalf("expected max concurrency 1, got %d", got)
	}
}

type groupTailProject struct{}

func (groupTailProject) Name() string { return "group-tail-project" }

func (groupTailProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "group-tail"}, nil
}

func (groupTailProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"bundle"}}}
}

func (groupTailProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name: "compile",
			Kind: project.KindOnce,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				_ = rt
				return nil
			},
		},
		{
			Name: "bundle",
			Kind: project.KindGroup,
			Deps: []string{"compile"},
		},
	}
}

func TestRunDoesNotStallWhenGroupTaskIsLastNode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	eng, err := New(groupTailProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Result.Success {
		t.Fatalf("expected success, got %+v", outcome.Result)
	}
}

type eventProject struct{}

func (eventProject) Name() string { return "event-project" }

func (eventProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "events"}, nil
}

func (eventProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"gen"}}}
}

func (eventProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:      "gen",
			Kind:      project.KindOnce,
			Cache:     true,
			Inputs:    project.Inputs{Files: []string{"input.txt"}},
			Outputs:   project.Outputs{Files: []string{"out.txt"}},
			Signature: "event-gen-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				if err := rt.RunCmd(ctx, "sh", "-c", "printf 'hello-event\\n'"); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(rt.Worktree, "out.txt"), []byte("done"), 0o644)
			},
		},
	}
}

func TestRunPublishesStructuredEvents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, err := New(eventProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEvents()
	if _, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	got := drainEvents(events)
	types := eventTypes(got)
	mustContainEventType(t, types, api.EventInstanceUpdated)
	mustContainEventType(t, types, api.EventRunStarted)
	mustContainEventType(t, types, api.EventTaskState)
	mustContainEventType(t, types, api.EventCacheMiss)
	mustContainEventType(t, types, api.EventLogLine)
	mustContainEventType(t, types, api.EventRunFinished)
}

func TestRunPublishesCacheHitOnSecondExecution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, err := New(eventProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEvents()
	if _, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	got := drainEvents(events)
	types := eventTypes(got)
	mustContainEventType(t, types, api.EventCacheHit)
}

func drainEvents(ch <-chan api.Event) []api.Event {
	out := make([]api.Event, 0)
	for {
		select {
		case evt := <-ch:
			out = append(out, evt)
		case <-time.After(25 * time.Millisecond):
			return out
		}
	}
}

func eventTypes(events []api.Event) []api.EventType {
	out := make([]api.EventType, 0, len(events))
	for _, evt := range events {
		out = append(out, evt.Type)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func mustContainEventType(t *testing.T, types []api.EventType, want api.EventType) {
	t.Helper()
	for _, got := range types {
		if got == want {
			return
		}
	}
	t.Fatalf("missing event type %q in %v", want, types)
}

type watchProject struct {
	aRuns       atomic.Int32
	bRuns       atomic.Int32
	serviceRuns atomic.Int32
}

func (p *watchProject) Name() string { return "watch-project" }

func (p *watchProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "watch"}, nil
}

func (p *watchProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"service", "b"}}}
}

func (p *watchProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:      "a",
			Kind:      project.KindOnce,
			Inputs:    project.Inputs{Files: []string{"a.txt"}},
			Outputs:   project.Outputs{Files: []string{"a.out"}},
			Cache:     true,
			Signature: "watch-a-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				p.aRuns.Add(1)
				return os.WriteFile(filepath.Join(rt.Worktree, "a.out"), []byte("a"), 0o644)
			},
		},
		{
			Name:      "b",
			Kind:      project.KindOnce,
			Inputs:    project.Inputs{Files: []string{"b.txt"}},
			Outputs:   project.Outputs{Files: []string{"b.out"}},
			Cache:     true,
			Signature: "watch-b-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				p.bRuns.Add(1)
				return os.WriteFile(filepath.Join(rt.Worktree, "b.out"), []byte("b"), 0o644)
			},
		},
		{
			Name:      "service",
			Kind:      project.KindService,
			Deps:      []string{"a"},
			Restart:   project.RestartOnInputChange,
			Signature: "watch-service-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				p.serviceRuns.Add(1)
				_, err := rt.StartServiceSpec(ctx, process.CommandSpec{
					Name: "sh",
					Args: []string{"-c", "trap 'exit 0' INT TERM; while true; do sleep 1; done"},
					Dir:  rt.Worktree,
					Env:  rt.Env,
				})
				return err
			},
		},
	}
}

func TestWatchRerunsOnlyAffectedSlice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("a1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "b.txt"), []byte("b1"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &watchProject{}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- eng.Watch(ctx, Request{Target: "dev", Worktree: worktree, Mode: api.ModeWatch, MaxParallel: 2})
	}()

	waitFor(t, 3*time.Second, func() bool {
		return p.aRuns.Load() == 1 && p.bRuns.Load() == 1 && p.serviceRuns.Load() == 1
	})

	if err := os.WriteFile(filepath.Join(worktree, "b.txt"), []byte("b2"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		return p.bRuns.Load() == 2
	})
	if got := p.aRuns.Load(); got != 1 {
		t.Fatalf("unexpected a reruns after b change: %d", got)
	}
	if got := p.serviceRuns.Load(); got != 1 {
		t.Fatalf("unexpected service restart after b change: %d", got)
	}

	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("a2"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok := waitForBool(4*time.Second, func() bool {
		return p.aRuns.Load() == 2 && p.serviceRuns.Load() == 2
	})
	if !ok {
		t.Fatalf("watch did not rerun expected slice: a=%d b=%d service=%d", p.aRuns.Load(), p.bRuns.Load(), p.serviceRuns.Load())
	}
	if got := p.bRuns.Load(); got != 2 {
		t.Fatalf("unexpected b reruns after a change: %d", got)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("watch returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for watch shutdown")
	}
}

type readinessProject struct{}

func (readinessProject) Name() string { return "readiness-project" }

func (readinessProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "readiness"}, nil
}

func (readinessProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"svc"}}}
}

func (readinessProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:         "svc",
			Kind:         project.KindService,
			Signature:    "svc-ready-v1",
			Ready:        project.ReadyFile(".ready/svc"),
			ReadyTimeout: 750 * time.Millisecond,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				readyPath := rt.Abs(".ready/svc")
				_ = os.Remove(readyPath)
				_, err := rt.StartServiceSpec(ctx, process.CommandSpec{
					Name: "sh",
					Args: []string{"-c", "trap 'rm -f \"$READY_PATH\"; exit 0' INT TERM; sleep 0.2; mkdir -p \"$(dirname \"$READY_PATH\")\"; : > \"$READY_PATH\"; while true; do sleep 1; done"},
					Dir:  rt.Worktree,
					Env: map[string]string{
						"READY_PATH": readyPath,
					},
				})
				return err
			},
		},
	}
}

type readinessTimeoutProject struct{}

func (readinessTimeoutProject) Name() string { return "readiness-timeout-project" }

func (readinessTimeoutProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "readiness-timeout"}, nil
}

func (readinessTimeoutProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"svc"}}}
}

func (readinessTimeoutProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:         "svc",
			Kind:         project.KindService,
			Signature:    "svc-timeout-v1",
			Ready:        project.ReadyFile(".ready/never"),
			ReadyTimeout: 250 * time.Millisecond,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_, err := rt.StartServiceSpec(ctx, process.CommandSpec{
					Name: "sh",
					Args: []string{"-c", "trap 'exit 0' INT TERM; while true; do sleep 1; done"},
					Dir:  rt.Worktree,
				})
				return err
			},
		},
	}
}

func TestServiceReadinessMustPassBeforeSuccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()

	eng, err := New(readinessProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	out, err := eng.Run(context.Background(), Request{Target: "dev", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, stopErr := instance.StopProcesses(out.Instance, ""); stopErr != nil {
			t.Fatalf("stop processes: %v", stopErr)
		}
	}()

	if elapsed := time.Since(started); elapsed < 175*time.Millisecond {
		t.Fatalf("service run completed before readiness delay elapsed: %s", elapsed)
	}

	status, err := instance.LoadStatus(worktree, out.Result.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	node := status.Nodes["svc"]
	if node.State != api.StateRunning {
		t.Fatalf("expected running state after readiness, got %q", node.State)
	}
	if node.PID <= 0 {
		t.Fatalf("expected tracked PID after readiness, got %d", node.PID)
	}
}

func TestServiceReadinessTimeoutFailsRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()

	eng, err := New(readinessTimeoutProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}

	out, err := eng.Run(context.Background(), Request{Target: "dev", Worktree: worktree, Mode: api.ModeCI})
	if err == nil {
		t.Fatal("expected readiness timeout error")
	}
	if out == nil {
		t.Fatal("expected partial outcome on readiness failure")
	}

	status, statusErr := instance.LoadStatus(worktree, out.Result.InstanceID)
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	node := status.Nodes["svc"]
	if node.State != api.StateFailed {
		t.Fatalf("expected failed state after readiness timeout, got %q", node.State)
	}
	if node.LastError == "" {
		t.Fatal("expected readiness failure message to be recorded")
	}
}

type binaryToolProject struct {
	tool project.BinaryTool
}

func (p *binaryToolProject) Name() string { return "binary-tool-project" }

func (p *binaryToolProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "binary-tool"}, nil
}

func (p *binaryToolProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"use_tool"}}}
}

func (p *binaryToolProject) Tasks() []project.Task {
	buildTask := p.tool.BuildTask()
	return []project.Task{
		buildTask,
		{
			Name: "use_tool",
			Kind: project.KindOnce,
			Deps: []string{buildTask.Name},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				return p.tool.RunSpec(ctx, rt, project.BinaryExecSpec{
					Args: []string{"hello"},
					Env: map[string]string{
						"OUT_FILE": rt.Abs("result.txt"),
					},
				})
			},
		},
	}
}

func TestBinaryToolBuildTaskCachesAndRestoresArtifact(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "tool-src.sh"), []byte("#!/bin/sh\necho \"$1\" > \"$OUT_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	p := &binaryToolProject{
		tool: project.BinaryTool{
			TaskName:    "build_mocktool",
			Description: "Build mock helper binary",
			Inputs:      project.Inputs{Files: []string{"tool-src.sh"}},
			Output:      ".devflow/tools/mocktool",
			Build: process.CommandSpec{
				Name: "sh",
				Args: []string{"-c", "mkdir -p .devflow/tools && cp tool-src.sh .devflow/tools/mocktool && chmod +x .devflow/tools/mocktool"},
			},
		},
	}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}

	first, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Result.CacheHits) != 0 {
		t.Fatalf("unexpected first-run cache hits: %v", first.Result.CacheHits)
	}
	if err := os.Remove(filepath.Join(worktree, ".devflow", "tools", "mocktool")); err != nil {
		t.Fatal(err)
	}

	second, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Result.CacheHits) != 1 || second.Result.CacheHits[0] != "build_mocktool" {
		t.Fatalf("unexpected second-run cache hits: %v", second.Result.CacheHits)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".devflow", "tools", "mocktool")); err != nil {
		t.Fatalf("expected cached binary to be restored: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "result.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "hello\n" {
		t.Fatalf("unexpected tool output %q", got)
	}
}

func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	if !waitForBool(timeout, fn) {
		t.Fatal("condition not met before timeout")
	}
}

func waitForBool(timeout time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}
