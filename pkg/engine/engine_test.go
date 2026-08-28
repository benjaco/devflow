package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/cache"
	"github.com/benjaco/devflow/pkg/fingerprint"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

type testProject struct{}

func TestDurationMillisecondsRecordsCompletedSubTickWork(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     int64
	}{
		{name: "negative", duration: -time.Nanosecond, want: 0},
		{name: "zero", duration: 0, want: 1},
		{name: "sub-millisecond", duration: time.Nanosecond, want: 1},
		{name: "one millisecond", duration: time.Millisecond, want: 1},
		{name: "whole milliseconds", duration: 2*time.Millisecond + 500*time.Microsecond, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := durationMilliseconds(tt.duration); got != tt.want {
				t.Fatalf("durationMilliseconds(%s) = %d, want %d", tt.duration, got, tt.want)
			}
		})
	}
}

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

type envPrecedenceProject struct{}

func (envPrecedenceProject) Name() string { return "env-precedence-project" }

func (envPrecedenceProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{
		Label: "env-precedence",
		Env: map[string]string{
			"APP_VALUE":    "dotenv-default",
			"DATABASE_URL": "postgres://dotenv/default",
		},
		Finalize: func(inst *api.Instance) error {
			inst.Env["DATABASE_URL"] = "postgres://managed/runtime"
			inst.Env["DEVFLOW_MANAGED"] = "managed"
			return nil
		},
	}, nil
}

func (envPrecedenceProject) Tasks() []project.Task {
	return []project.Task{{
		Name:        "check",
		Kind:        project.KindOnce,
		RequiredEnv: []string{"APP_VALUE"},
		Inputs:      project.Inputs{Env: []string{"DATABASE_URL"}},
	}}
}

func (envPrecedenceProject) Targets() []project.Target {
	return []project.Target{{Name: "check", RootTasks: []string{"check"}}}
}

func TestProcessEnvOverridesDefaultsAndManagedEnvWinsLast(t *testing.T) {
	worktree := t.TempDir()
	t.Setenv("APP_VALUE", "ci-process")
	t.Setenv("DATABASE_URL", "postgres://ci/process")
	eng, err := New(envPrecedenceProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := eng.Run(context.Background(), Request{Target: "check", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if got := outcome.Instance.Env["APP_VALUE"]; got != "ci-process" {
		t.Fatalf("process env did not override dotenv/default value: %q", got)
	}
	if got := outcome.Instance.Env["DATABASE_URL"]; got != "postgres://managed/runtime" {
		t.Fatalf("managed database URL did not win last: %q", got)
	}
	if got := outcome.Instance.Env["DEVFLOW_MANAGED"]; got != "managed" {
		t.Fatalf("missing managed runtime value: %q", got)
	}
}

func TestRunUsesCacheOnSecondExecution(t *testing.T) {
	isolateEngineUserCache(t)
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
	if len(first.Result.CacheMisses) != 1 || first.Result.CacheMisses[0] != "gen" || len(first.Result.Nodes) != 1 {
		t.Fatalf("unexpected first-run cache result: %+v", first.Result)
	}
	if timing := first.Result.Nodes[0].Cache; timing == nil || timing.Outcome != "miss" || timing.TotalDurationMs <= 0 || first.Result.Nodes[0].State != api.StateDone || first.Result.Nodes[0].DurationMs <= 0 {
		t.Fatalf("missing first-run node/cache timing: %+v", first.Result.Nodes[0])
	}
	second, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Result.CacheHits) != 1 || second.Result.CacheHits[0] != "gen" {
		t.Fatalf("unexpected cache hits on second run: %v", second.Result.CacheHits)
	}
	if len(second.Result.CacheMisses) != 0 || len(second.Result.Nodes) != 1 {
		t.Fatalf("unexpected second-run cache result: %+v", second.Result)
	}
	if timing := second.Result.Nodes[0].Cache; timing == nil || timing.Outcome != "hit" || timing.TotalDurationMs <= 0 || second.Result.Nodes[0].State != api.StateCached || second.Result.Nodes[0].DurationMs <= 0 {
		t.Fatalf("missing second-run node/cache timing: %+v", second.Result.Nodes[0])
	}
}

func TestCacheKeyMatchesExecutionTaskKey(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, err := New(testProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	keyResult, err := eng.CacheKey(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if keyResult.Key == "" || len(keyResult.TaskKeys) != 1 || keyResult.TaskKeys[0].Task != "gen" {
		t.Fatalf("unexpected target cache key: %+v", keyResult)
	}
	outcome, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Result.Nodes) != 1 || outcome.Result.Nodes[0].LastRunKey != keyResult.TaskKeys[0].Key {
		t.Fatalf("planned key does not match execution: planned=%+v run=%+v", keyResult, outcome.Result.Nodes)
	}
}

func TestFilteredInputsSkipCompileOnIrrelevantEditAndRerunOnRelevantEdit(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	sourcePath := filepath.Join(worktree, "internal", "api", "users.go")
	outputPath := filepath.Join(worktree, "docs", "swagger.json")
	writeSource := func(content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(sourcePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sourcePath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSource(`package api

// @Summary List users
func ListUsers() {
	println("first")
}

// User is returned by the API.
type User struct {
	ID int
}
`)

	var runs atomic.Int32
	var filterApplications atomic.Int32
	p := project.Define(func(ctx context.Context, b *project.Builder) error {
		_ = ctx
		b.Name("filtered-input-engine-project")
		b.CacheNamespace("filtered-input-engine-project-" + filepath.Base(worktree))
		semanticFilter := project.CombineContentFilters(
			project.GoCommentLinesStartingWith("@"),
			project.GoStructDeclarations(),
		)
		filter := project.ContentFilter("counted:"+semanticFilter.Signature, func(ctx context.Context, rt *project.Runtime, file project.FileContent) ([]byte, error) {
			filterApplications.Add(1)
			return semanticFilter.Apply(ctx, rt, file)
		})
		swagger := b.Task("swagger").
			Run(func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				data, err := os.ReadFile(rt.Abs("internal/api/users.go"))
				if err != nil {
					return err
				}
				if strings.Contains(string(data), "broken()") {
					return fmt.Errorf("compile should not run for non-filtered edits")
				}
				runs.Add(1)
				if err := os.MkdirAll(filepath.Dir(rt.Abs("docs/swagger.json")), 0o755); err != nil {
					return err
				}
				return os.WriteFile(rt.Abs("docs/swagger.json"), []byte("compiled"), 0o644)
			}).
			Inputs(project.Filtered(project.Glob("internal/**/*.go"), filter)).
			Outputs("docs/swagger.json")
		b.Target("docs", swagger)
		return nil
	})
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}

	first, err := eng.Run(context.Background(), Request{Target: "docs", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Result.CacheHits) != 0 {
		t.Fatalf("unexpected first-run cache hits: %v", first.Result.CacheHits)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("compile task runs after first run = %d, want 1", got)
	}
	if got := filterApplications.Load(); got != 1 {
		t.Fatalf("filter applications after first run = %d, want 1", got)
	}

	unchanged, err := eng.Run(context.Background(), Request{Target: "docs", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Result.CacheHits) != 1 || unchanged.Result.CacheHits[0] != "swagger" {
		t.Fatalf("expected cache hit for unchanged source, got %v", unchanged.Result.CacheHits)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("compile task ran for unchanged source: runs=%d", got)
	}
	if got := filterApplications.Load(); got != 1 {
		t.Fatalf("unchanged source should reuse in-memory filtered hash, applications=%d", got)
	}

	writeSource(`package api

// @Summary List users
func ListUsers() {
	broken()
}

// User is returned by the API.
type User struct {
	ID int
}
`)
	if err := os.Remove(outputPath); err != nil {
		t.Fatal(err)
	}
	irrelevant, err := eng.Run(context.Background(), Request{Target: "docs", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatalf("irrelevant edit should restore cache instead of compiling non-compiling source: %v", err)
	}
	if len(irrelevant.Result.CacheHits) != 1 || irrelevant.Result.CacheHits[0] != "swagger" {
		t.Fatalf("expected filtered cache hit after irrelevant edit, got %v", irrelevant.Result.CacheHits)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("compile task ran for irrelevant edit: runs=%d", got)
	}
	if got := filterApplications.Load(); got != 2 {
		t.Fatalf("changed source should be filtered once before cache restore, applications=%d", got)
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("expected cached output to be restored: %v", err)
	}

	writeSource(`package api

// @Summary Search users
func ListUsers() {
	println("second")
}

// User is returned by the API.
type User struct {
	ID int
}
`)
	relevant, err := eng.Run(context.Background(), Request{Target: "docs", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if len(relevant.Result.CacheHits) != 0 {
		t.Fatalf("expected relevant filtered edit to miss cache, got hits %v", relevant.Result.CacheHits)
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("compile task runs after relevant edit = %d, want 2", got)
	}
	if got := filterApplications.Load(); got != 3 {
		t.Fatalf("relevant edit should be filtered before rerun, applications=%d", got)
	}
}

type taskLogAttemptProject struct {
	attempt atomic.Int32
}

func (p *taskLogAttemptProject) Name() string { return "task-log-attempt-project" }

func (p *taskLogAttemptProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "task-log-attempt"}, nil
}

func (p *taskLogAttemptProject) Tasks() []project.Task {
	return []project.Task{{
		Name: "prepare",
		Kind: project.KindOnce,
		Run: func(ctx context.Context, rt *project.Runtime) error {
			_ = ctx
			if p.attempt.Add(1) == 1 {
				rt.EmitLogLine("stderr", "first attempt failed")
				return fmt.Errorf("first attempt failed")
			}
			rt.EmitLogLine("stdout", "second attempt running")
			return nil
		},
	}}
}

func (p *taskLogAttemptProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"prepare"}}}
}

func TestTaskLogTruncatedBeforeCustomRunAttempt(t *testing.T) {
	worktree := t.TempDir()
	p := &taskLogAttemptProject{}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	first, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err == nil {
		t.Fatal("expected first run to fail")
	}
	logPath := instance.LogPath(worktree, first.Instance.ID, "prepare")
	firstLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(firstLog), "first attempt failed") {
		t.Fatalf("expected first attempt log, got %q", firstLog)
	}

	if _, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	secondLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(secondLog), "first attempt failed") {
		t.Fatalf("expected second attempt log to drop old failure, got %q", secondLog)
	}
	if !strings.Contains(string(secondLog), "second attempt running") {
		t.Fatalf("expected current attempt output, got %q", secondLog)
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

type cancelSiblingProject struct{}

func (cancelSiblingProject) Name() string { return "cancel-sibling-project" }

func (cancelSiblingProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "cancel-sibling"}, nil
}

func (cancelSiblingProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"join"}}}
}

func (cancelSiblingProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name: "fail_fast",
			Kind: project.KindOnce,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				return fmt.Errorf("boom")
			},
		},
		{
			Name: "slow",
			Kind: project.KindOnce,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				return rt.RunCmdSpec(ctx, process.CommandSpec{
					Name: testCommandPath(),
					Args: []string{"serve"},
					Dir:  rt.Worktree,
				})
			},
		},
		{
			Name: "join",
			Kind: project.KindOnce,
			Deps: []string{"fail_fast", "slow"},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				return nil
			},
		},
	}
}

func TestCanceledSiblingUsesCanceledState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	eng, err := New(cancelSiblingProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	out, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI, MaxParallel: 2})
	if err == nil {
		t.Fatal("expected run to fail")
	}
	if out == nil {
		t.Fatal("expected partial outcome")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected first task error to be preserved, got %v", err)
	}
	status, err := instance.LoadStatus(worktree, out.Result.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if got := status.Nodes["fail_fast"].State; got != api.StateFailed {
		t.Fatalf("expected fail_fast to be failed, got %q", got)
	}
	if got := status.Nodes["slow"].State; got != api.StateCanceled {
		t.Fatalf("expected slow to be canceled, got %q", got)
	}
	if got := status.Nodes["slow"].LastError; got != "canceled" {
		t.Fatalf("expected slow last error to be canceled, got %q", got)
	}
}

type migrationNeededProject struct {
	appRan *atomic.Bool
}

func (migrationNeededProject) Name() string { return "migration-needed-project" }

func (migrationNeededProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "migration-needed"}, nil
}

func (migrationNeededProject) Targets() []project.Target {
	return []project.Target{{Name: "up", RootTasks: []string{"app"}}}
}

func (p migrationNeededProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name: "db_prepare",
			Kind: project.KindOnce,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				return testMigrationNeededError{message: "migration needed"}
			},
		},
		{
			Name: "app",
			Kind: project.KindOnce,
			Deps: []string{"db_prepare"},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				if p.appRan != nil {
					p.appRan.Store(true)
				}
				return nil
			},
		},
	}
}

type testMigrationNeededError struct {
	message string
}

func (e testMigrationNeededError) Error() string {
	return e.message
}

func (e testMigrationNeededError) MigrationNeeded() bool {
	return true
}

func TestMigrationNeededErrorUsesMigrationNeededState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	appRan := &atomic.Bool{}
	eng, err := New(migrationNeededProject{appRan: appRan}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	out, err := eng.Run(context.Background(), Request{Target: "up", Worktree: worktree, Mode: api.ModeWatch})
	if err == nil {
		t.Fatal("expected run to stop for migration-needed task")
	}
	if out == nil {
		t.Fatal("expected partial outcome")
	}
	if out.Result.FailedNode != "db_prepare" {
		t.Fatalf("expected failed node to point at migration-needed task, got %q", out.Result.FailedNode)
	}
	status, err := instance.LoadStatus(worktree, out.Result.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if got := status.Nodes["db_prepare"].State; got != api.StateMigrationNeeded {
		t.Fatalf("expected db_prepare to be migration_needed, got %q", got)
	}
	if got := status.Nodes["db_prepare"].LastError; got != "migration needed" {
		t.Fatalf("expected migration-needed last error, got %q", got)
	}
	if got := status.Nodes["app"].State; got != api.StateBlocked {
		t.Fatalf("expected downstream task to be blocked, got %q", got)
	}
	if got := status.Nodes["app"].LastError; !strings.Contains(got, "db_prepare") {
		t.Fatalf("expected downstream blocking reason to name db_prepare, got %q", got)
	}
	if appRan.Load() {
		t.Fatal("downstream task should not run when migration is needed")
	}
}

func TestMigrationNeededMessageUsesMigrationNeededState(t *testing.T) {
	err := fmt.Errorf("prisma schema changed without a new migration; generate one with GeneratePrismaMigration before preparing the database")
	if got := classifyTaskError(context.Background(), err); got != api.StateMigrationNeeded {
		t.Fatalf("expected migration-needed state from Prisma guard message, got %q", got)
	}
}

type interactiveProject struct{}

func (interactiveProject) Name() string { return "interactive-project" }

func (interactiveProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "interactive"}, nil
}

func (interactiveProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"prompt"}}}
}

func (interactiveProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name: "prompt",
			Kind: project.KindOnce,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				return rt.RunCmdSpec(ctx, process.CommandSpec{
					Name:        buildPromptCLIForEngine(tRepoRoot()),
					Dir:         rt.Worktree,
					Env:         rt.Env,
					Interactive: true,
					Prompts: []process.PromptSpec{
						{Pattern: "Continue? [y/N]: ", Prompt: "Continue?", Kind: process.PromptConfirm},
						{Pattern: "Name: ", Prompt: "Name", Kind: process.PromptText},
					},
				})
			},
		},
	}
}

func TestRunInteractiveTaskAnswersViaInstanceFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	eng, err := New(interactiveProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEvents()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for evt := range events {
			if evt.Type != api.EventInteractionReq {
				continue
			}
			answer := "y"
			if evt.PromptKind == string(process.PromptText) {
				answer = "Ada"
			}
			if err := instance.WriteInteractionAnswer(worktree, evt.InstanceID, evt.PromptID, answer); err != nil {
				t.Errorf("write interaction answer: %v", err)
				return
			}
		}
	}()
	outcome, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Result.Success {
		t.Fatalf("expected success, got %+v", outcome.Result)
	}
	waitFor(t, 3*time.Second, func() bool {
		lines, err := os.ReadFile(instance.LogPath(worktree, outcome.Instance.ID, "prompt"))
		return err == nil && string(lines) != "" && strings.Contains(string(lines), "Hello, Ada")
	})
}

func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("failed to resolve repo root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

var (
	engineTestCommandOnce sync.Once
	engineTestCommandPath string
)

func testCommandPath() string {
	engineTestCommandOnce.Do(func() {
		root := repoRoot()
		bin := filepath.Join(os.TempDir(), "devflow-testcmd-test"+engineTestExeSuffix())
		cmd := exec.Command("go", "build", "-o", bin, "./internal/testutil/testcmd")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			panic("build test command: " + err.Error() + "\n" + string(out))
		}
		engineTestCommandPath = bin
	})
	return engineTestCommandPath
}

func testServiceSpec(rt *project.Runtime) process.CommandSpec {
	return process.CommandSpec{
		Name: testCommandPath(),
		Args: []string{"serve"},
		Dir:  rt.Worktree,
		Env:  rt.Env,
	}
}

func installFakeDebugCommands(t *testing.T, binDir string) {
	t.Helper()
	src := testCommandPath()
	for _, name := range []string{"go", "dlv"} {
		dst := filepath.Join(binDir, name+engineTestExeSuffix())
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func countDebugStarts(recordPath string) int {
	data, err := os.ReadFile(recordPath)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "fake-dlv ")
}

func nodeRunningWithDebugPort(t *testing.T, worktree, instanceID, task string) bool {
	t.Helper()
	status, err := instance.LoadStatus(worktree, instanceID)
	if err != nil {
		return false
	}
	node := status.Nodes[task]
	return node.State == api.StateRunning && node.PID > 0 && node.Debug != nil && node.Debug.Port > 0 && node.Debug.Attach.Port == node.Debug.Port
}

func readFileForFailure(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return string(data)
}

func readStatusForFailure(worktree, instanceID string) string {
	status, err := instance.LoadStatus(worktree, instanceID)
	if err != nil {
		return err.Error()
	}
	var parts []string
	for name, node := range status.Nodes {
		parts = append(parts, fmt.Sprintf("%s=%s error=%q pid=%d debug=%+v", name, node.State, node.LastError, node.PID, node.Debug))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func isolateEngineUserCache(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "LocalAppData"))
}

func buildPromptCLIForEngine(root string) string {
	bin := filepath.Join(os.TempDir(), "devflow-promptcli-test"+engineTestExeSuffix())
	cmd := exec.Command("go", "build", "-o", bin, "./internal/testutil/promptcli")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("build prompt cli: " + err.Error() + "\n" + string(out))
	}
	return bin
}

func engineTestExeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func tRepoRoot() string {
	return repoRoot()
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
				if err := rt.RunCmd(ctx, testCommandPath(), "emit", "hello-event"); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(rt.Worktree, "out.txt"), []byte("done"), 0o644)
			},
		},
	}
}

func TestRunPublishesStructuredEvents(t *testing.T) {
	isolateEngineUserCache(t)
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
	isolateEngineUserCache(t)
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
	cacheNamespace string
	aRuns          atomic.Int32
	bRuns          atomic.Int32
	serviceRuns    atomic.Int32
}

func (p *watchProject) Name() string { return "watch-project" }

func (p *watchProject) CacheNamespace() string {
	if p.cacheNamespace != "" {
		return p.cacheNamespace
	}
	return p.Name()
}

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
				_, err := rt.StartServiceSpec(ctx, testServiceSpec(rt))
				return err
			},
		},
	}
}

func TestWatchRerunsOnlyAffectedSlice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "LocalAppData"))
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("a1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "b.txt"), []byte("b1"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &watchProject{cacheNamespace: "watch-project-" + filepath.Base(worktree)}
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

	waitFor(t, 6*time.Second, func() bool {
		return p.aRuns.Load() == 1 && p.bRuns.Load() == 1 && p.serviceRuns.Load() == 1
	})
	waitForEngineWatchReady(t, worktree)

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

func TestWatchInputPathsUseTargetClosureDeclaredInputs(t *testing.T) {
	g, err := graph.New([]project.Task{
		{
			Name:   "client",
			Kind:   project.KindOnce,
			Inputs: project.Inputs{Paths: []string{"package.json"}, Globs: []string{"src/**/*.ts"}},
		},
		{
			Name:   "migrations",
			Kind:   project.KindOnce,
			Inputs: project.Inputs{Dirs: []string{"prisma/migrations"}, Files: []string{"prisma/schema.prisma"}},
		},
		{
			Name: "swagger",
			Kind: project.KindOnce,
			Inputs: project.Inputs{Filtered: []project.FilteredInput{
				project.Filtered(project.Glob("api/**/*.go"), project.CombineContentFilters(project.GoCommentLinesStartingWith("@"), project.GoStructDeclarations())),
			}},
		},
		{
			Name:   "app",
			Kind:   project.KindService,
			Deps:   []string{"client", "migrations", "swagger"},
			Inputs: project.Inputs{Files: []string{"src/server.ts"}},
		},
		{
			Name:   "unrelated",
			Kind:   project.KindOnce,
			Inputs: project.Inputs{Paths: []string{"node_modules"}},
		},
	}, []project.Target{{Name: "up", RootTasks: []string{"app"}}})
	if err != nil {
		t.Fatal(err)
	}
	order, err := g.TargetClosure("up")
	if err != nil {
		t.Fatal(err)
	}
	eng := &Engine{graph: g}
	got := eng.watchInputPaths(order)
	want := []string{"api", "package.json", "prisma/migrations", "prisma/schema.prisma", "src"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected watch paths: got %v want %v", got, want)
	}
}

func TestWatchCycleEventsReportChangedFilesAndAffectedTasks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "LocalAppData"))
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("a1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "b.txt"), []byte("b1"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &watchProject{cacheNamespace: "watch-project-" + filepath.Base(worktree)}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEvents()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- eng.Watch(ctx, Request{Target: "dev", Worktree: worktree, Mode: api.ModeWatch, MaxParallel: 2})
	}()

	waitFor(t, 6*time.Second, func() bool {
		return p.aRuns.Load() == 1 && p.serviceRuns.Load() == 1
	})
	waitForEngineWatchReady(t, worktree)

	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("a2"), 0o644); err != nil {
		t.Fatal(err)
	}

	var sawStart bool
	var sawDone bool
	waitFor(t, 4*time.Second, func() bool {
		for {
			select {
			case evt := <-events:
				if evt.Type == api.EventWatchCycleStart && stringSliceHas(evt.Files, "a.txt") {
					sawStart = len(evt.AffectedTasks) == 1 && evt.AffectedTasks[0] == "a"
				}
				if evt.Type == api.EventWatchCycleDone && stringSliceHas(evt.Files, "a.txt") {
					sawDone = len(evt.AffectedTasks) == 1 && evt.AffectedTasks[0] == "a" && evt.Success != nil && *evt.Success
				}
			default:
				return sawStart && sawDone && p.aRuns.Load() == 2 && p.serviceRuns.Load() == 2
			}
		}
	})

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

type flushWatchProject struct {
	runs      atomic.Int32
	failOnBad bool
	noInputs  bool
}

func (p *flushWatchProject) Name() string { return "flush-watch-project" }

func (p *flushWatchProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "flush-watch"}, nil
}

func (p *flushWatchProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"gen"}}}
}

func (p *flushWatchProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:    "gen",
			Kind:    project.KindOnce,
			Inputs:  flushWatchInputs(p.noInputs),
			Outputs: project.Outputs{Files: []string{"out.txt"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				p.runs.Add(1)
				data, err := os.ReadFile(filepath.Join(rt.Worktree, "input.txt"))
				if err != nil {
					return err
				}
				if p.failOnBad && strings.TrimSpace(string(data)) == "bad" {
					return fmt.Errorf("bad input")
				}
				return os.WriteFile(filepath.Join(rt.Worktree, "out.txt"), data, 0o644)
			},
		},
	}
}

func flushWatchInputs(noInputs bool) project.Inputs {
	if noInputs {
		return project.Inputs{}
	}
	return project.Inputs{Files: []string{"input.txt"}}
}

func TestWatchFlushAckAfterFileChangeRerun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &flushWatchProject{}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- eng.Watch(ctx, Request{Target: "dev", Worktree: worktree, Mode: api.ModeWatch})
	}()
	instanceID := waitForEngineWatchReady(t, worktree)

	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	requestID := writeEngineFlushRequest(t, worktree, instanceID)
	result := waitForEngineFlushAck(t, worktree, instanceID, requestID)
	if !result.Success || !result.Synced {
		t.Fatalf("expected successful synced flush, got %+v", result)
	}
	if got := p.runs.Load(); got != 2 {
		t.Fatalf("expected rerun before ack, got %d runs", got)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "out.txt")); err != nil || string(data) != "v2" {
		t.Fatalf("expected rerun output v2, got %q err=%v", string(data), err)
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

func TestWatchFlushAckWithNoUserChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &flushWatchProject{}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- eng.Watch(ctx, Request{Target: "dev", Worktree: worktree, Mode: api.ModeWatch})
	}()
	instanceID := waitForEngineWatchReady(t, worktree)

	requestID := writeEngineFlushRequest(t, worktree, instanceID)
	result := waitForEngineFlushAck(t, worktree, instanceID, requestID)
	if !result.Success || !result.Synced {
		t.Fatalf("expected successful sync-only flush, got %+v", result)
	}
	if got := p.runs.Load(); got != 1 {
		t.Fatalf("expected no rerun for sync-only flush, got %d runs", got)
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

func TestWatchFlushAckWithNoDeclaredInputs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &flushWatchProject{noInputs: true}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- eng.Watch(ctx, Request{Target: "dev", Worktree: worktree, Mode: api.ModeWatch})
	}()
	instanceID := waitForEngineWatchReady(t, worktree)

	requestID := writeEngineFlushRequest(t, worktree, instanceID)
	result := waitForEngineFlushAck(t, worktree, instanceID, requestID)
	if !result.Success || !result.Synced {
		t.Fatalf("expected successful sync-only flush with no declared inputs, got %+v", result)
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

func TestWatchFlushAckReportsTaskFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("good"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &flushWatchProject{failOnBad: true}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- eng.Watch(ctx, Request{Target: "dev", Worktree: worktree, Mode: api.ModeWatch})
	}()
	instanceID := waitForEngineWatchReady(t, worktree)

	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	requestID := writeEngineFlushRequest(t, worktree, instanceID)
	result := waitForEngineFlushAck(t, worktree, instanceID, requestID)
	if result.Success {
		t.Fatalf("expected failed flush, got %+v", result)
	}
	if len(result.Issues) != 1 || result.Issues[0].Task != "gen" || result.Issues[0].Kind != "task_failed" {
		t.Fatalf("unexpected issues: %+v", result.Issues)
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

type flushUnreadyServiceProject struct{}

func (flushUnreadyServiceProject) Name() string { return "flush-unready-service-project" }

func (flushUnreadyServiceProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "flush-unready-service"}, nil
}

func (flushUnreadyServiceProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"svc"}}}
}

func (flushUnreadyServiceProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:         "svc",
			Kind:         project.KindService,
			ReadyTimeout: 100 * time.Millisecond,
			Ready: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				_ = rt
				return fmt.Errorf("not ready")
			},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_, err := rt.StartServiceSpec(ctx, testServiceSpec(rt))
				return err
			},
		},
	}
}

func TestWatchFlushAckReportsServiceReadinessFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	eng, err := New(flushUnreadyServiceProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- eng.Watch(ctx, Request{Target: "dev", Worktree: worktree, Mode: api.ModeWatch})
	}()
	instanceID := waitForEngineWatchReady(t, worktree)

	requestID := writeEngineFlushRequest(t, worktree, instanceID)
	result := waitForEngineFlushAck(t, worktree, instanceID, requestID)
	if result.Success {
		t.Fatalf("expected failed flush, got %+v", result)
	}
	if len(result.Services) != 1 || result.Services[0].Task != "svc" || result.Services[0].Ready {
		t.Fatalf("unexpected services: %+v", result.Services)
	}
	if len(result.Issues) != 1 || result.Issues[0].Task != "svc" || result.Issues[0].Kind != "service_unhealthy" {
		t.Fatalf("unexpected issues: %+v", result.Issues)
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

type watchServiceChainProject struct {
	buildRuns atomic.Int32
	apiRuns   atomic.Int32
	uiRuns    atomic.Int32
}

type watchGeneratedOutputProject struct {
	bundleRuns  atomic.Int32
	serviceRuns atomic.Int32
}

func (p *watchServiceChainProject) Name() string { return "watch-service-chain-project" }

func (p *watchServiceChainProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "watch-chain"}, nil
}

func (p *watchServiceChainProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"ui"}}}
}

func (p *watchServiceChainProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:      "build",
			Kind:      project.KindOnce,
			Inputs:    project.Inputs{Files: []string{"input.txt"}},
			Outputs:   project.Outputs{Files: []string{"build.out"}},
			Cache:     true,
			Signature: "watch-chain-build-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				p.buildRuns.Add(1)
				return os.WriteFile(filepath.Join(rt.Worktree, "build.out"), []byte("build"), 0o644)
			},
		},
		{
			Name:      "api",
			Kind:      project.KindService,
			Deps:      []string{"build"},
			Restart:   project.RestartOnInputChange,
			Signature: "watch-chain-api-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				p.apiRuns.Add(1)
				_, err := rt.StartServiceSpec(ctx, testServiceSpec(rt))
				return err
			},
		},
		{
			Name:      "ui",
			Kind:      project.KindService,
			Deps:      []string{"api"},
			Restart:   project.RestartOnInputChange,
			Signature: "watch-chain-ui-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				p.uiRuns.Add(1)
				_, err := rt.StartServiceSpec(ctx, testServiceSpec(rt))
				return err
			},
		},
	}
}

func TestWatchDoesNotPropagateAcrossServiceDependencies(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &watchServiceChainProject{}
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
		return p.buildRuns.Load() == 1 && p.apiRuns.Load() == 1 && p.uiRuns.Load() == 1
	})
	waitForEngineWatchReady(t, worktree)
	time.Sleep(500 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 4*time.Second, func() bool {
		return p.buildRuns.Load() == 2 && p.apiRuns.Load() == 2
	})
	if got := p.uiRuns.Load(); got != 1 {
		t.Fatalf("expected downstream service not to restart from service-to-service propagation, got %d", got)
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

type watchBlockedWarmupProject struct{}

func (watchBlockedWarmupProject) Name() string { return "watch-blocked-warmup-project" }

func (watchBlockedWarmupProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "watch-blocked-warmup"}, nil
}

func (watchBlockedWarmupProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"serve"}}}
}

func (watchBlockedWarmupProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:   "schema",
			Kind:   project.KindOnce,
			Inputs: project.Inputs{Files: []string{"schema.txt"}},
		},
		{
			Name:         "prepare",
			Kind:         project.KindWarmup,
			Deps:         []string{"schema"},
			AllowInWatch: false,
		},
		{
			Name:    "serve",
			Kind:    project.KindService,
			Deps:    []string{"prepare"},
			Restart: project.RestartOnInputChange,
		},
	}
}

func TestWatchCascadeDoesNotRunPastWarmupBlockedInWatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	eng, err := New(watchBlockedWarmupProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}

	order, changed := eng.affectedWatchOrder("dev", []string{"schema.txt"})
	if got, want := strings.Join(changed, ","), "schema"; got != want {
		t.Fatalf("changed=%s want=%s", got, want)
	}
	if got, want := strings.Join(order, ","), "schema"; got != want {
		t.Fatalf("order=%s want=%s", got, want)
	}
}

type watchBlockedWarmupRuntimeProject struct {
	sourceRuns  atomic.Int32
	prepareRuns atomic.Int32
	serveRuns   atomic.Int32
}

func (p *watchBlockedWarmupRuntimeProject) Name() string {
	return "watch-blocked-warmup-runtime-project"
}

func (p *watchBlockedWarmupRuntimeProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "watch-blocked-warmup-runtime"}, nil
}

func (p *watchBlockedWarmupRuntimeProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"serve"}}}
}

func (p *watchBlockedWarmupRuntimeProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:    "source",
			Kind:    project.KindOnce,
			Inputs:  project.Inputs{Files: []string{"source.txt"}},
			Outputs: project.Outputs{Files: []string{"source.out"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				p.sourceRuns.Add(1)
				data, err := os.ReadFile(filepath.Join(rt.Worktree, "source.txt"))
				if err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(rt.Worktree, "source.out"), data, 0o644)
			},
		},
		{
			Name:         "prepare",
			Kind:         project.KindWarmup,
			Deps:         []string{"source"},
			AllowInWatch: false,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				_ = rt
				p.prepareRuns.Add(1)
				return nil
			},
		},
		{
			Name:    "serve",
			Kind:    project.KindService,
			Deps:    []string{"prepare"},
			Restart: project.RestartOnInputChange,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				p.serveRuns.Add(1)
				_, err := rt.StartServiceSpec(ctx, testServiceSpec(rt))
				return err
			},
		},
	}
}

func TestWatchRunDoesNotRestartServicePastWarmupBlockedInWatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "source.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &watchBlockedWarmupRuntimeProject{}
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
		return p.sourceRuns.Load() == 1 && p.prepareRuns.Load() == 1 && p.serveRuns.Load() == 1
	})
	time.Sleep(500 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(worktree, "source.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 4*time.Second, func() bool {
		return p.sourceRuns.Load() == 2
	})
	if got := p.prepareRuns.Load(); got != 1 {
		t.Fatalf("unexpected blocked warmup rerun: %d", got)
	}
	if got := p.serveRuns.Load(); got != 1 {
		t.Fatalf("unexpected service restart past blocked warmup: %d", got)
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

type watchRestartNeverCascadeProject struct{}

func (watchRestartNeverCascadeProject) Name() string { return "watch-restart-never-cascade-project" }

func (watchRestartNeverCascadeProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "watch-restart-never"}, nil
}

func (watchRestartNeverCascadeProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"ui"}}}
}

func (watchRestartNeverCascadeProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:   "source",
			Kind:   project.KindOnce,
			Inputs: project.Inputs{Files: []string{"source.txt"}},
		},
		{
			Name:    "api",
			Kind:    project.KindService,
			Deps:    []string{"source"},
			Restart: project.RestartNever,
		},
		{
			Name:                      "ui",
			Kind:                      project.KindService,
			Deps:                      []string{"api"},
			Restart:                   project.RestartOnInputChange,
			WatchRestartOnServiceDeps: true,
		},
	}
}

func TestWatchCascadeDoesNotRunPastRestartNeverService(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	eng, err := New(watchRestartNeverCascadeProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}

	order, changed := eng.affectedWatchOrder("dev", []string{"source.txt"})
	if got, want := strings.Join(changed, ","), "source"; got != want {
		t.Fatalf("changed=%s want=%s", got, want)
	}
	if got, want := strings.Join(order, ","), "source"; got != want {
		t.Fatalf("order=%s want=%s", got, want)
	}
}

type watchMixedCascadeProject struct{}

func (watchMixedCascadeProject) Name() string { return "watch-mixed-cascade-project" }

func (watchMixedCascadeProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "watch-mixed-cascade"}, nil
}

func (watchMixedCascadeProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"blocked_service", "allowed_service"}}}
}

func (watchMixedCascadeProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:   "source",
			Kind:   project.KindOnce,
			Inputs: project.Inputs{Files: []string{"source.txt"}},
		},
		{
			Name:         "blocked_prepare",
			Kind:         project.KindWarmup,
			Deps:         []string{"source"},
			AllowInWatch: false,
		},
		{
			Name:    "blocked_service",
			Kind:    project.KindService,
			Deps:    []string{"blocked_prepare"},
			Restart: project.RestartOnInputChange,
		},
		{
			Name: "allowed_build",
			Kind: project.KindOnce,
			Deps: []string{"source"},
		},
		{
			Name:    "allowed_service",
			Kind:    project.KindService,
			Deps:    []string{"allowed_build"},
			Restart: project.RestartOnInputChange,
		},
	}
}

func TestWatchCascadeKeepsAllowedSiblingBranchWhenAnotherBranchBlocked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	eng, err := New(watchMixedCascadeProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}

	order, changed := eng.affectedWatchOrder("dev", []string{"source.txt"})
	if got, want := strings.Join(changed, ","), "source"; got != want {
		t.Fatalf("changed=%s want=%s", got, want)
	}
	if got, want := strings.Join(order, ","), "source,allowed_build,allowed_service"; got != want {
		t.Fatalf("order=%s want=%s", got, want)
	}
}

type watchRestartAlwaysProject struct {
	changedRuns atomic.Int32
	alwaysRuns  atomic.Int32
}

func (p *watchRestartAlwaysProject) Name() string { return "watch-restart-always-project" }

func (p *watchRestartAlwaysProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "watch-restart-always"}, nil
}

func (p *watchRestartAlwaysProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"changed", "always"}}}
}

func (p *watchRestartAlwaysProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:   "changed",
			Kind:   project.KindOnce,
			Inputs: project.Inputs{Files: []string{"changed.txt"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				_ = rt
				p.changedRuns.Add(1)
				return nil
			},
		},
		{
			Name:    "always",
			Kind:    project.KindService,
			Restart: project.RestartAlways,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				p.alwaysRuns.Add(1)
				_, err := rt.StartServiceSpec(ctx, testServiceSpec(rt))
				return err
			},
		},
	}
}

func TestWatchCascadeIncludesRestartAlwaysServiceOnAnyTargetChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	eng, err := New(&watchRestartAlwaysProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}

	order, changed := eng.affectedWatchOrder("dev", []string{"changed.txt"})
	if got, want := strings.Join(changed, ","), "changed"; got != want {
		t.Fatalf("changed=%s want=%s", got, want)
	}
	if got, want := strings.Join(order, ","), "always,changed"; got != want {
		t.Fatalf("order=%s want=%s", got, want)
	}
}

func TestWatchRunRestartsRestartAlwaysServiceOnAnyTargetChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "changed.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &watchRestartAlwaysProject{}
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
		return p.changedRuns.Load() == 1 && p.alwaysRuns.Load() == 1
	})
	waitForEngineWatchReady(t, worktree)

	if err := os.WriteFile(filepath.Join(worktree, "changed.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 4*time.Second, func() bool {
		return p.changedRuns.Load() == 2 && p.alwaysRuns.Load() == 2
	})

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

func (p *watchGeneratedOutputProject) Name() string { return "watch-generated-output-project" }

func (p *watchGeneratedOutputProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "watch-generated"}, nil
}

func (p *watchGeneratedOutputProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"svc"}}}
}

func (p *watchGeneratedOutputProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:      "bundle",
			Kind:      project.KindOnce,
			Cache:     true,
			Inputs:    project.Inputs{Files: []string{"src.txt"}},
			Outputs:   project.Outputs{Files: []string{"generated/out.txt"}},
			Signature: "watch-generated-bundle-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				p.bundleRuns.Add(1)
				data, err := os.ReadFile(filepath.Join(rt.Worktree, "src.txt"))
				if err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Join(rt.Worktree, "generated"), 0o755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(rt.Worktree, "generated", "out.txt"), data, 0o644)
			},
		},
		{
			Name:      "svc",
			Kind:      project.KindService,
			Deps:      []string{"bundle"},
			Inputs:    project.Inputs{Dirs: []string{"generated"}},
			Restart:   project.RestartOnInputChange,
			Signature: "watch-generated-svc-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				p.serviceRuns.Add(1)
				_, err := rt.StartServiceSpec(ctx, testServiceSpec(rt))
				return err
			},
		},
	}
}

func TestWatchOutputSuppressorFiltersOutputFilesAndDirs(t *testing.T) {
	g, err := graph.New((&watchGeneratedOutputProject{}).Tasks(), []project.Target{{Name: "dev", RootTasks: []string{"svc"}}})
	if err != nil {
		t.Fatal(err)
	}
	suppressor := watchOutputSuppressor{
		files: map[string]time.Time{},
		dirs:  map[string]time.Time{},
	}
	suppressor.Record(g, []string{"bundle"}, time.Minute)

	filtered := suppressor.Filter([]string{"generated/out.txt", "generated", "src.txt"})
	if got := strings.Join(filtered, ","); got != "src.txt" {
		t.Fatalf("unexpected filtered files %q", got)
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
					Name: testCommandPath(),
					Args: []string{"serve"},
					Dir:  rt.Worktree,
					Env: map[string]string{
						"TESTCMD_READY_FILE":     readyPath,
						"TESTCMD_READY_DELAY_MS": "200",
					},
				})
				return err
			},
			AfterReady: func(ctx context.Context, rt *project.Runtime) error {
				return os.WriteFile(rt.Abs(".ready/committed"), []byte("ready\n"), 0o600)
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
				_, err := rt.StartServiceSpec(ctx, testServiceSpec(rt))
				return err
			},
			AfterReady: func(ctx context.Context, rt *project.Runtime) error {
				return os.WriteFile(rt.Abs("after-ready-must-not-run"), []byte("unexpected\n"), 0o600)
			},
		},
	}
}

func TestCIModeServiceReadinessPassesThenStopsService(t *testing.T) {
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

	if elapsed := time.Since(started); elapsed < 175*time.Millisecond {
		t.Fatalf("service run completed before readiness delay elapsed: %s", elapsed)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, ".ready", "committed")); err != nil || string(data) != "ready\n" {
		t.Fatalf("after-ready hook was not committed: data=%q err=%v", data, err)
	}

	status, err := instance.LoadStatus(worktree, out.Result.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	node := status.Nodes["svc"]
	if node.State != api.StateStopped {
		t.Fatalf("expected stopped state after CI readiness probe, got %q", node.State)
	}
	if node.PID != 0 {
		t.Fatalf("expected cleared PID after CI readiness probe, got %d", node.PID)
	}
	loaded, err := instance.Load(worktree, out.Result.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Processes) != 0 {
		t.Fatalf("expected no tracked services after CI readiness probe, got %+v", loaded.Processes)
	}
}

type earlyExitServiceHandle struct {
	err error
}

func (h earlyExitServiceHandle) PID() int    { return 0 }
func (h earlyExitServiceHandle) Alive() bool { return false }
func (h earlyExitServiceHandle) Wait() error { return h.err }
func (h earlyExitServiceHandle) Stop() error { return nil }

type earlyExitDebugProject struct{}

func (earlyExitDebugProject) Name() string { return "early-exit-debug-project" }
func (earlyExitDebugProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "early-debug"}, nil
}
func (earlyExitDebugProject) Targets() []project.Target {
	return []project.Target{{Name: "debug", RootTasks: []string{"backend_debug"}}}
}
func (earlyExitDebugProject) Tasks() []project.Task {
	return []project.Task{{
		Name:         "backend_debug",
		Kind:         project.KindDebugService,
		ReadyTimeout: time.Second,
		Ready: func(ctx context.Context, _ *project.Runtime) error {
			<-ctx.Done()
			return ctx.Err()
		},
		Run: func(_ context.Context, rt *project.Runtime) error {
			if err := os.WriteFile(rt.LogPath, []byte("starting delve\nError: broken pipe while accepting DAP connection\n"), 0o600); err != nil {
				return err
			}
			rt.RegisterServiceHandle(earlyExitServiceHandle{err: errors.New("delve disconnected: broken pipe")})
			return nil
		},
	}}
}

func TestEarlyDebugExitBecomesFailedWithBoundedContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := t.TempDir()
	eng, err := New(earlyExitDebugProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	out, err := eng.Run(context.Background(), Request{Target: "debug", Worktree: worktree, Mode: api.ModeDev})
	if err == nil || out == nil {
		t.Fatalf("expected early debug exit failure, outcome=%+v err=%v", out, err)
	}
	node := out.Result.Nodes[0]
	if node.State != api.StateFailed || strings.Contains(string(node.State), "pending") {
		t.Fatalf("dead debug process retained a non-terminal state: %+v", node)
	}
	if !strings.Contains(node.LastError, "broken pipe") {
		t.Fatalf("early exit cause was not preserved: %+v", node)
	}
	if len(node.FailureExcerpts) == 0 || !strings.Contains(strings.Join(node.FailureExcerpts[0].Lines, "\n"), "broken pipe") {
		t.Fatalf("bounded failure context was not exposed in status: %+v", node.FailureExcerpts)
	}
	if node.FailureExcerpts[0].Reason == "process-exit-tail" {
		t.Fatalf("generic fallback replaced a recognized error marker: %+v", node.FailureExcerpts)
	}
	data, marshalErr := json.Marshal(node)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if len(data) > 80*1024 {
		t.Fatalf("debug failure status was not bounded: %d bytes", len(data))
	}
}

type genericEarlyExitProject struct{}

func (genericEarlyExitProject) Name() string { return "generic-early-exit-project" }
func (genericEarlyExitProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{
		Label: "generic-early-exit",
		Env:   map[string]string{"SERVICE_TOKEN": "do-not-expose"},
	}, nil
}
func (genericEarlyExitProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"broken_service"}}}
}
func (genericEarlyExitProject) Tasks() []project.Task {
	return []project.Task{{
		Name:         "broken_service",
		Kind:         project.KindService,
		ReadyTimeout: time.Second,
		Ready: func(ctx context.Context, _ *project.Runtime) error {
			<-ctx.Done()
			return ctx.Err()
		},
		Run: func(_ context.Context, rt *project.Runtime) error {
			line := "stderr: broken_service: synthetic early exit before readiness do-not-expose postgresql://user:db-secret@db.example/app\n"
			if err := os.WriteFile(rt.LogPath, []byte(line), 0o600); err != nil {
				return err
			}
			rt.RegisterServiceHandle(earlyExitServiceHandle{err: errors.New("exit status 17")})
			return nil
		},
	}}
}

func TestGenericEarlyServiceExitIncludesBoundedRedactedFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := t.TempDir()
	eng, err := New(genericEarlyExitProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := eng.Run(context.Background(), Request{Target: "dev", Worktree: worktree, Mode: api.ModeDev})
	if runErr == nil || out == nil {
		t.Fatalf("expected generic early exit failure: outcome=%+v err=%v", out, runErr)
	}
	node := out.Result.Nodes[0]
	if node.State != api.StateFailed || !strings.Contains(node.LastError, "exit status 17") {
		t.Fatalf("generic process error missing: %+v", node)
	}
	if len(node.FailureExcerpts) != 1 || node.FailureExcerpts[0].Reason != "process-exit-tail" {
		t.Fatalf("generic fallback missing from status: %+v", node.FailureExcerpts)
	}
	encoded, err := json.Marshal(out.Result)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, "synthetic early exit before readiness") {
		t.Fatalf("failure result omitted the useful service output: %s", text)
	}
	for _, secret := range []string{"do-not-expose", "db-secret", "postgresql://"} {
		if strings.Contains(text, secret) {
			t.Fatalf("failure result leaked %q: %s", secret, text)
		}
	}
}

type postReadyExitDebugProject struct{}

func (postReadyExitDebugProject) Name() string { return "post-ready-exit-debug-project" }
func (postReadyExitDebugProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "post-ready-debug"}, nil
}
func (postReadyExitDebugProject) Targets() []project.Target {
	return []project.Target{{Name: "debug", RootTasks: []string{"backend_debug"}}}
}
func (postReadyExitDebugProject) Tasks() []project.Task {
	return []project.Task{{
		Name: "backend_debug",
		Kind: project.KindDebugService,
		Run: func(_ context.Context, rt *project.Runtime) error {
			if err := os.WriteFile(rt.LogPath, []byte("Delve ready\nError: DAP client disconnected: broken pipe\n"), 0o600); err != nil {
				return err
			}
			rt.RegisterServiceHandle(earlyExitServiceHandle{err: errors.New("DAP client disconnected: broken pipe")})
			return nil
		},
	}}
}

func TestWatchDetectsDebugExitAfterReadiness(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := t.TempDir()
	eng, err := New(postReadyExitDebugProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- eng.Watch(ctx, Request{Target: "debug", Worktree: worktree, Mode: api.ModeWatch})
	}()
	id, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if !waitForBool(3*time.Second, func() bool {
		state, loadErr := instance.LoadStatus(worktree, id)
		if loadErr != nil {
			return false
		}
		node := state.Nodes["backend_debug"]
		return node.State == api.StateFailed && strings.Contains(node.LastError, "broken pipe") && len(node.FailureExcerpts) > 0
	}) {
		t.Fatalf("dead debug adapter remained non-terminal: %s", readStatusForFailure(worktree, id))
	}
	select {
	case err := <-done:
		t.Fatalf("detached watch exited instead of retaining inspectable failed state: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not stop after debug failure test")
	}
}

type genericServiceHandle struct {
	done    chan struct{}
	stop    sync.Once
	stopped atomic.Bool
}

type lifecycleServiceProject struct {
	mu      sync.Mutex
	handles map[string][]*genericServiceHandle
}

func (p *lifecycleServiceProject) Name() string { return "lifecycle-service-project" }
func (p *lifecycleServiceProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "lifecycle-services"}, nil
}
func (p *lifecycleServiceProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"backend", "frontend"}}}
}
func (p *lifecycleServiceProject) Tasks() []project.Task {
	service := func(name string) project.Task {
		return project.Task{
			Name:  name,
			Kind:  project.KindService,
			Ready: func(context.Context, *project.Runtime) error { return nil },
			Run: func(_ context.Context, rt *project.Runtime) error {
				handle := newGenericServiceHandle()
				p.mu.Lock()
				p.handles[name] = append(p.handles[name], handle)
				p.mu.Unlock()
				rt.RegisterServiceHandle(handle)
				return nil
			},
		}
	}
	return []project.Task{service("backend"), service("frontend")}
}
func (p *lifecycleServiceProject) snapshots(name string) []*genericServiceHandle {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*genericServiceHandle(nil), p.handles[name]...)
}

func TestLifecycleControllerRestartsAndStopsOnlySelectedService(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := t.TempDir()
	p := &lifecycleServiceProject{handles: map[string][]*genericServiceHandle{}}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	controller := NewLifecycleController()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := eng.Run(ctx, Request{
			Target:              "dev",
			Worktree:            worktree,
			Mode:                api.ModeDev,
			LifecycleController: controller,
		})
		done <- runErr
	}()

	if !waitForBool(3*time.Second, func() bool {
		return len(p.snapshots("backend")) == 1 && len(p.snapshots("frontend")) == 1
	}) {
		t.Fatal("independent services did not become ready")
	}
	firstBackend := p.snapshots("backend")[0]
	frontend := p.snapshots("frontend")[0]
	restarted, err := controller.Restart(context.Background(), "backend")
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.Stopped || !restarted.Ready || restarted.Previous.Generation == 0 || restarted.Current.Generation <= restarted.Previous.Generation {
		t.Fatalf("restart did not report a ready replacement identity: %+v", restarted)
	}
	if !firstBackend.stopped.Load() {
		t.Fatal("previous backend handle was not stopped")
	}
	if frontend.stopped.Load() || len(p.snapshots("frontend")) != 1 {
		t.Fatal("independent frontend was changed by backend restart")
	}

	stopped, err := controller.Stop(context.Background(), "backend")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Previous.Generation != restarted.Current.Generation {
		t.Fatalf("stop affected the wrong backend generation: stop=%+v restart=%+v", stopped, restarted)
	}
	if !stopped.Stopped {
		t.Fatalf("live backend stop was not confirmed: %+v", stopped)
	}
	if frontend.stopped.Load() {
		t.Fatal("independent frontend was changed by backend stop")
	}

	restartedFromStopped, err := controller.Restart(context.Background(), "backend")
	if err != nil {
		t.Fatal(err)
	}
	if restartedFromStopped.Stopped || !restartedFromStopped.Ready || restartedFromStopped.Previous.Generation != 0 || restartedFromStopped.Current.Generation <= restarted.Current.Generation {
		t.Fatalf("stopped service did not get a new ready generation: %+v", restartedFromStopped)
	}
	concurrent := make(chan ServiceLifecycleResult, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			result, restartErr := controller.Restart(context.Background(), "backend")
			concurrent <- result
			errs <- restartErr
		}()
	}
	generations := map[uint64]bool{}
	for range 2 {
		if restartErr := <-errs; restartErr != nil {
			t.Fatalf("serialized concurrent restart failed: %v", restartErr)
		}
		result := <-concurrent
		if !result.Ready || result.Current.Generation <= restartedFromStopped.Current.Generation {
			t.Fatalf("concurrent restart was not a ready later generation: %+v", result)
		}
		generations[result.Current.Generation] = true
	}
	if len(generations) != 2 || frontend.stopped.Load() {
		t.Fatalf("concurrent restarts were not serialized or changed frontend: generations=%v", generations)
	}

	cancel()
	select {
	case runErr := <-done:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Fatal(runErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("engine did not stop after lifecycle test cancellation")
	}
}

type lifecycleProcessProject struct{}

func (lifecycleProcessProject) Name() string { return "lifecycle-process-project" }
func (lifecycleProcessProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "lifecycle-processes"}, nil
}
func (lifecycleProcessProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"backend_debug", "frontend"}}}
}
func (lifecycleProcessProject) Tasks() []project.Task {
	service := func(name string) project.Task {
		return project.Task{
			Name: name,
			Kind: project.KindService,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_, err := rt.StartServiceSpec(ctx, testServiceSpec(rt))
				return err
			},
		}
	}
	return []project.Task{service("backend_debug"), service("frontend")}
}

func TestLifecycleControllerReplacesRealProcessCrossPlatform(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := t.TempDir()
	eng, err := New(lifecycleProcessProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	controller := NewLifecycleController()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := eng.Run(ctx, Request{Target: "dev", Worktree: worktree, Mode: api.ModeDev, LifecycleController: controller})
		done <- runErr
	}()
	var backendPID, frontendPID int
	if !waitForBool(5*time.Second, func() bool {
		id, _, idErr := instance.IDForWorktree(worktree)
		if idErr != nil {
			return false
		}
		state, loadErr := instance.LoadStatus(worktree, id)
		if loadErr != nil {
			return false
		}
		backendPID = state.Nodes["backend_debug"].PID
		frontendPID = state.Nodes["frontend"].PID
		return backendPID > 0 && frontendPID > 0
	}) {
		t.Fatal("real independent service processes did not start")
	}
	change, err := controller.Restart(context.Background(), "backend_debug")
	if err != nil {
		t.Fatal(err)
	}
	if change.Previous.PID != backendPID || change.Current.PID <= 0 || change.Current.PID == backendPID || !change.Ready {
		t.Fatalf("backend process identity was not replaced after readiness: %+v", change)
	}
	if instance.ProcessAlive(backendPID) {
		t.Fatalf("old backend process %d remained alive", backendPID)
	}
	if !instance.ProcessAlive(frontendPID) {
		t.Fatalf("independent frontend process %d was stopped", frontendPID)
	}

	cancel()
	select {
	case runErr := <-done:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Fatal(runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process lifecycle engine did not stop")
	}
	if instance.ProcessAlive(change.Current.PID) || instance.ProcessAlive(frontendPID) {
		t.Fatalf("service processes leaked after cancellation: backend=%v frontend=%v", instance.ProcessAlive(change.Current.PID), instance.ProcessAlive(frontendPID))
	}
}

type restartFailureProject struct {
	backendRuns atomic.Int32
	frontend    atomic.Pointer[genericServiceHandle]
}

func (p *restartFailureProject) Name() string { return "restart-failure-project" }
func (p *restartFailureProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "restart-failure"}, nil
}
func (p *restartFailureProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"backend", "frontend"}}}
}
func (p *restartFailureProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name: "backend",
			Kind: project.KindService,
			Ready: func(context.Context, *project.Runtime) error {
				if p.backendRuns.Load() > 1 {
					return errors.New("backend application readiness failed")
				}
				return nil
			},
			Run: func(_ context.Context, rt *project.Runtime) error {
				attempt := p.backendRuns.Add(1)
				if attempt > 1 {
					if err := os.WriteFile(rt.LogPath, []byte("Error: backend bind failed during restart\n"), 0o600); err != nil {
						return err
					}
				}
				rt.RegisterServiceHandle(newGenericServiceHandle())
				return nil
			},
		},
		{
			Name:  "frontend",
			Kind:  project.KindService,
			Ready: func(context.Context, *project.Runtime) error { return nil },
			Run: func(_ context.Context, rt *project.Runtime) error {
				handle := newGenericServiceHandle()
				p.frontend.Store(handle)
				rt.RegisterServiceHandle(handle)
				return nil
			},
		},
	}
}

func TestLifecycleRestartReadinessFailureIsTerminalAndPreservesIndependentService(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := t.TempDir()
	p := &restartFailureProject{}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	controller := NewLifecycleController()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := eng.Run(ctx, Request{Target: "dev", Worktree: worktree, Mode: api.ModeDev, LifecycleController: controller})
		done <- runErr
	}()
	if !waitForBool(3*time.Second, func() bool { return p.backendRuns.Load() == 1 && p.frontend.Load() != nil }) {
		t.Fatal("restart failure fixture did not reach initial readiness")
	}
	if _, err := controller.Restart(context.Background(), "backend"); err == nil || !strings.Contains(err.Error(), "readiness failed") {
		t.Fatalf("restart readiness failure was reported as success: %v", err)
	}
	id, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	state, err := instance.LoadStatus(worktree, id)
	if err != nil {
		t.Fatal(err)
	}
	backend := state.Nodes["backend"]
	if backend.State != api.StateFailed || len(backend.FailureExcerpts) == 0 || !strings.Contains(strings.Join(backend.FailureExcerpts[0].Lines, "\n"), "bind failed") {
		t.Fatalf("restart failure lacked bounded terminal diagnostics: %+v", backend)
	}
	if p.frontend.Load().stopped.Load() {
		t.Fatal("backend restart failure stopped independent frontend")
	}
	cancel()
	select {
	case runErr := <-done:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Fatal(runErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restart failure fixture did not clean up")
	}
}

func newGenericServiceHandle() *genericServiceHandle {
	return &genericServiceHandle{done: make(chan struct{})}
}

func (h *genericServiceHandle) PID() int    { return 0 }
func (h *genericServiceHandle) Alive() bool { return !h.stopped.Load() }
func (h *genericServiceHandle) Wait() error {
	<-h.done
	return nil
}
func (h *genericServiceHandle) Stop() error {
	h.stop.Do(func() {
		h.stopped.Store(true)
		close(h.done)
	})
	return nil
}

type genericServiceProject struct {
	handle *genericServiceHandle
}

func (p *genericServiceProject) Name() string { return "generic-service-project" }
func (p *genericServiceProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "generic-service"}, nil
}
func (p *genericServiceProject) Targets() []project.Target {
	return []project.Target{{Name: "ci", RootTasks: []string{"managed"}}}
}
func (p *genericServiceProject) Tasks() []project.Task {
	return []project.Task{{
		Name:  "managed",
		Kind:  project.KindService,
		Ready: func(context.Context, *project.Runtime) error { return nil },
		Run: func(_ context.Context, rt *project.Runtime) error {
			p.handle = newGenericServiceHandle()
			rt.RegisterServiceHandle(p.handle)
			return nil
		},
	}}
}

func TestCIModeSupervisesNonProcessServiceHandle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := t.TempDir()
	p := &genericServiceProject{}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	out, err := eng.Run(context.Background(), Request{Target: "ci", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if p.handle == nil || !p.handle.stopped.Load() {
		t.Fatal("CI did not stop the registered non-process service")
	}
	if len(out.Instance.Processes) != 0 {
		t.Fatalf("non-process service was persisted as an OS process: %+v", out.Instance.Processes)
	}
	status, err := instance.LoadStatus(worktree, out.Instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if node := status.Nodes["managed"]; node.State != api.StateStopped || node.PID != 0 {
		t.Fatalf("unexpected non-process service status: %+v", node)
	}
}

func TestWatchFlushAcceptsLiveNonProcessServiceHandle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := t.TempDir()
	p := &genericServiceProject{}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- eng.Watch(ctx, Request{Target: "ci", Worktree: worktree, Mode: api.ModeWatch})
	}()
	instanceID := waitForEngineWatchReady(t, worktree)

	requestID := writeEngineFlushRequest(t, worktree, instanceID)
	result := waitForEngineFlushAck(t, worktree, instanceID, requestID)
	if !result.Success || len(result.Services) != 1 {
		t.Fatalf("unexpected non-process service flush: %+v", result)
	}
	service := result.Services[0]
	if service.Task != "managed" || service.PID != 0 || !service.Alive || !service.Ready {
		t.Fatalf("unexpected non-process service health: %+v", service)
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
	if p.handle == nil || !p.handle.stopped.Load() {
		t.Fatal("watch shutdown did not stop the non-process service")
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
	if _, err := os.Stat(filepath.Join(worktree, "after-ready-must-not-run")); !os.IsNotExist(err) {
		t.Fatalf("after-ready hook ran after readiness failure: %v", err)
	}
}

func TestGoDebugServiceWatchBuildsDelveAndRestarts(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	fakeBin := t.TempDir()
	installFakeDebugCommands(t, fakeBin)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	recordPath := filepath.Join(worktree, "debug-record.log")

	sourcePath := filepath.Join(worktree, "cmd", "api", "main.go")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "schema.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := project.Define(func(ctx context.Context, b *project.Builder) error {
		_ = ctx
		b.Name("debug-watch-project")
		b.Env("DEVFLOW_FAKE_DEBUG_RECORD", recordPath)
		generate := b.Task("generate").
			Run(func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				return project.WriteFile(rt, "generated.txt", []byte("generated\n"), 0o644)
			}).
			Inputs("schema.txt").
			Outputs("generated.txt")
		debug := b.GoDebugService("api_debug").
			Package("./cmd/api").
			DebugPort("debug_api").
			BuildFlags("-tags=dev").
			Inputs("cmd/api").
			DependsOn(generate).
			Args("--config", ".devflow/dev.yaml").
			ReadyTimeout(2 * time.Second)
		b.Target("debug", debug)
		return nil
	})
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- eng.Watch(ctx, Request{Target: "debug", Worktree: worktree, Mode: api.ModeWatch})
	}()
	instanceID := waitForEngineWatchReady(t, worktree)
	if !waitForBool(6*time.Second, func() bool {
		return countDebugStarts(recordPath) >= 1 && nodeRunningWithDebugPort(t, worktree, instanceID, "api_debug")
	}) {
		t.Fatalf("debug service did not start: record=%q status=%s", readFileForFailure(recordPath), readStatusForFailure(worktree, instanceID))
	}

	if err := os.WriteFile(sourcePath, []byte("package main\nfunc main() { println(\"changed\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitForBool(6*time.Second, func() bool {
		return countDebugStarts(recordPath) >= 2 && nodeRunningWithDebugPort(t, worktree, instanceID, "api_debug")
	}) {
		t.Fatalf("debug service did not restart: record=%q status=%s", readFileForFailure(recordPath), readStatusForFailure(worktree, instanceID))
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("watch did not exit after cancel")
	}
}

func TestGoDebugServiceRunsWithRealDelveInCIProbe(t *testing.T) {
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skip("dlv not installed")
	}
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "go.mod"), []byte("module debug-smoke\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(worktree, "cmd", "api", "main.go")
	if err := os.MkdirAll(filepath.Dir(mainPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mainPath, []byte(`package main

import "time"

func main() {
	for {
		time.Sleep(time.Second)
	}
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	p := project.Define(func(ctx context.Context, b *project.Builder) error {
		_ = ctx
		b.Name("real-delve-debug-project")
		debug := b.GoDebugService("api_debug").
			Package("./cmd/api").
			DebugPort("debug_api").
			Inputs("go.mod", "cmd/api").
			ReadyTimeout(10 * time.Second)
		b.Target("debug", debug)
		return nil
	})
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}

	out, err := eng.Run(context.Background(), Request{Target: "debug", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		logPath := ""
		if out != nil {
			logPath = instance.LogPath(worktree, out.Instance.ID, "api_debug")
		}
		t.Fatalf("real dlv debug run failed: %v\nlog:\n%s", err, readFileForFailure(logPath))
	}
	if !out.Result.Success {
		t.Fatalf("expected success, got %+v", out.Result)
	}
	status, err := instance.LoadStatus(worktree, out.Instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	node := status.Nodes["api_debug"]
	if node.State != api.StateStopped {
		t.Fatalf("CI mode should stop debug service after readiness, got %q", node.State)
	}
	if node.Debug == nil || node.Debug.Port == 0 || node.Debug.Attach.Port != node.Debug.Port {
		t.Fatalf("missing debug status metadata: %+v", node.Debug)
	}
	if node.PID != 0 {
		t.Fatalf("expected stopped debug service PID to be cleared, got %d", node.PID)
	}
}

func TestGoDebugServiceWatchRestartsRealDelveAfterSourceEdit(t *testing.T) {
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skip("dlv not installed")
	}
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "go.mod"), []byte("module real-delve-watch\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(worktree, "cmd", "api", "main.go")
	if err := os.MkdirAll(filepath.Dir(mainPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDebugHTTPProgram(t, mainPath, "v1")

	p := project.Define(func(ctx context.Context, b *project.Builder) error {
		_ = ctx
		b.Name("real-delve-watch-project")
		apiPort := b.Port("api")
		debug := b.GoDebugService("api_debug").
			Package("./cmd/api").
			DebugPort("debug_api").
			Env("PORT", apiPort).
			Inputs("go.mod", "cmd/api").
			ReadyHTTP("api", "/health", 200).
			ReadyTimeout(20 * time.Second)
		b.Target("debug", debug)
		return nil
	})
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	watchDone := make(chan struct{})
	var watchErr error
	var watchErrMu sync.Mutex
	watchError := func() error {
		watchErrMu.Lock()
		defer watchErrMu.Unlock()
		return watchErr
	}
	go func() {
		err := eng.Watch(ctx, Request{Target: "debug", Worktree: worktree, Mode: api.ModeWatch})
		watchErrMu.Lock()
		watchErr = err
		watchErrMu.Unlock()
		close(watchDone)
	}()
	defer func() {
		cancel()
		select {
		case <-watchDone:
			err := watchError()
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("debug watch returned error during cleanup: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("debug watch did not stop during cleanup")
		}
	}()

	instanceID := waitForEngineWatchReadyWithin(t, worktree, 30*time.Second)
	inst, err := instance.Resolve(worktree, "")
	if err != nil {
		t.Fatal(err)
	}
	apiPort := inst.Ports["api"]
	if apiPort == 0 {
		t.Fatalf("expected api port to be allocated: %+v", inst.Ports)
	}
	logPath := instance.LogPath(worktree, instanceID, "api_debug")
	failIfWatchExited(t, watchDone, watchError, worktree, instanceID)

	var firstPID int
	if !waitForBool(30*time.Second, func() bool {
		failIfWatchExited(t, watchDone, watchError, worktree, instanceID)
		status, err := instance.LoadStatus(worktree, instanceID)
		if err != nil {
			return false
		}
		node := status.Nodes["api_debug"]
		if node.State != api.StateRunning || node.PID == 0 || node.Debug == nil || node.Debug.Port == 0 || node.Debug.Attach.Port != node.Debug.Port {
			return false
		}
		if !tcpPortOpen(node.Debug.Port) {
			return false
		}
		body, ok := httpBodyForPort(apiPort)
		if !ok || body != "v1" {
			return false
		}
		firstPID = node.PID
		return strings.Count(readFileForFailure(logPath), "debug: starting Delve on") >= 1
	}) {
		t.Fatalf("real Delve debug service did not start on %s: status=%s log=%s", runtime.GOOS, readStatusForFailure(worktree, instanceID), readFileForFailure(logPath))
	}

	writeDebugHTTPProgram(t, mainPath, "v2")
	if !waitForBool(40*time.Second, func() bool {
		failIfWatchExited(t, watchDone, watchError, worktree, instanceID)
		status, err := instance.LoadStatus(worktree, instanceID)
		if err != nil {
			return false
		}
		node := status.Nodes["api_debug"]
		if node.State != api.StateRunning || node.PID == 0 || node.PID == firstPID || node.Debug == nil || node.Debug.Port == 0 || node.Debug.Attach.Port != node.Debug.Port {
			return false
		}
		if !tcpPortOpen(node.Debug.Port) {
			return false
		}
		body, ok := httpBodyForPort(apiPort)
		return ok && body == "v2"
	}) {
		t.Fatalf("real Delve debug service did not restart after source edit on %s: firstPID=%d status=%s log=%s", runtime.GOOS, firstPID, readStatusForFailure(worktree, instanceID), readFileForFailure(logPath))
	}
}

func writeDebugHTTPProgram(t *testing.T, path, body string) {
	t.Helper()
	source := fmt.Sprintf(`package main

import (
	"fmt"
	"net/http"
	"os"
)

const response = %q

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "0"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, response)
	})
	if err := http.ListenAndServe("127.0.0.1:"+port, mux); err != nil {
		panic(err)
	}
}
`, body)
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
}

func httpBodyForPort(port int) (string, bool) {
	client := &http.Client{Timeout: 250 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false
	}
	return string(data), true
}

func tcpPortOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 150*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func failIfWatchExited(t *testing.T, done <-chan struct{}, errFn func() error, worktree, instanceID string) {
	t.Helper()
	select {
	case <-done:
		err := errFn()
		t.Fatalf("debug watch exited early: %v status=%s", err, readStatusForFailure(worktree, instanceID))
	default:
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
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	source := filepath.Join(worktree, "cmd", "mocktool", "main.go")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte(`package main

import (
	"fmt"
	"os"
)

func main() {
	arg := ""
	if len(os.Args) > 1 {
		arg = os.Args[1]
	}
	if err := os.WriteFile(os.Getenv("OUT_FILE"), []byte(fmt.Sprintln(arg)), 0o644); err != nil {
		panic(err)
	}
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	output := ".devflow/tools/mocktool" + engineTestExeSuffix()

	p := &binaryToolProject{
		tool: project.BinaryTool{
			TaskName:    "build_mocktool",
			Description: "Build mock helper binary",
			Inputs:      project.Inputs{Files: []string{"cmd/mocktool/main.go"}},
			Output:      output,
			Build: process.CommandSpec{
				Name: "go",
				Args: []string{"build", "-o", output, "cmd/mocktool/main.go"},
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
	if err := os.Remove(filepath.Join(worktree, output)); err != nil {
		t.Fatal(err)
	}

	second, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Result.CacheHits) != 1 || second.Result.CacheHits[0] != "build_mocktool" {
		t.Fatalf("unexpected second-run cache hits: %v", second.Result.CacheHits)
	}
	if _, err := os.Stat(filepath.Join(worktree, output)); err != nil {
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

type overrideCacheProject struct{}

func (overrideCacheProject) Name() string { return "override-cache-project" }

func (overrideCacheProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{
		Label: "override-cache",
		Env: map[string]string{
			"SEMANTIC_KEY": "v1",
			"PAYLOAD":      "override-payload",
		},
	}, nil
}

func (overrideCacheProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"gen"}}}
}

func (overrideCacheProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:    "gen",
			Kind:    project.KindOnce,
			Cache:   true,
			Inputs:  project.Inputs{Files: []string{"missing-input.txt"}},
			Outputs: project.Outputs{Files: []string{"out.txt"}},
			CacheKeyOverride: func(ctx context.Context, rt *project.Runtime) (string, error) {
				_ = ctx
				return "semantic:" + rt.Env["SEMANTIC_KEY"], nil
			},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				return os.WriteFile(rt.Abs("out.txt"), []byte(rt.Env["PAYLOAD"]), 0o644)
			},
		},
	}
}

func TestCacheKeyOverrideBypassesAutomaticInputsAndRestores(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()

	eng, err := New(overrideCacheProject{}, worktree)
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
	if got, err := os.ReadFile(filepath.Join(worktree, "out.txt")); err != nil || string(got) != "override-payload" {
		t.Fatalf("unexpected first run output %q err=%v", string(got), err)
	}
	if err := os.Remove(filepath.Join(worktree, "out.txt")); err != nil {
		t.Fatal(err)
	}
	second, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Result.CacheHits) != 1 || second.Result.CacheHits[0] != "gen" {
		t.Fatalf("unexpected second-run cache hits: %v", second.Result.CacheHits)
	}
	if got, err := os.ReadFile(filepath.Join(worktree, "out.txt")); err != nil || string(got) != "override-payload" {
		t.Fatalf("expected cached override output to be restored, got %q err=%v", string(got), err)
	}
}

type stampedInstallProject struct {
	runs       atomic.Int32
	touchInput bool
}

func (p *stampedInstallProject) Name() string { return "stamped-install-project" }

func (p *stampedInstallProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "stamped"}, nil
}

func (p *stampedInstallProject) Targets() []project.Target {
	return []project.Target{{Name: "up", RootTasks: []string{"npm_install"}}}
}

func (p *stampedInstallProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:    "npm_install",
			Kind:    project.KindOnce,
			Stamp:   true,
			Inputs:  project.Inputs{Files: []string{"package-lock.json"}},
			Outputs: project.Outputs{Dirs: []string{"node_modules"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				p.runs.Add(1)
				if err := os.MkdirAll(filepath.Join(rt.Worktree, "node_modules"), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(rt.Worktree, "node_modules", ".installed"), []byte("ok"), 0o644); err != nil {
					return err
				}
				if p.touchInput {
					path := filepath.Join(rt.Worktree, "package-lock.json")
					now := time.Now().Add(2 * time.Second)
					if err := os.Chtimes(path, now, now); err != nil {
						return err
					}
				}
				return nil
			},
		},
	}
}

func TestStampedTaskSkipsWhenInputKeyAndLocalOutputsMatch(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "package-lock.json"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &stampedInstallProject{}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background(), Request{Target: "up", Worktree: worktree, Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	if got := p.runs.Load(); got != 1 {
		t.Fatalf("runs after first execution = %d, want 1", got)
	}
	store := cache.NewNamespaced(instance.CacheRoot(), project.CacheNamespace(p))
	if entries, err := store.List(); err != nil || len(entries) != 0 {
		t.Fatalf("stamped task should not create global cache entries, entries=%v err=%v", entries, err)
	}
	if _, err := eng.Run(context.Background(), Request{Target: "up", Worktree: worktree, Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	if got := p.runs.Load(); got != 1 {
		t.Fatalf("runs after matching stamp = %d, want 1", got)
	}
	if err := os.RemoveAll(filepath.Join(worktree, "node_modules")); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background(), Request{Target: "up", Worktree: worktree, Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	if got := p.runs.Load(); got != 2 {
		t.Fatalf("runs after local output removal = %d, want 2", got)
	}
	if err := os.WriteFile(filepath.Join(worktree, "package-lock.json"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background(), Request{Target: "up", Worktree: worktree, Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	if got := p.runs.Load(); got != 3 {
		t.Fatalf("runs after input change = %d, want 3", got)
	}
}

func TestStampedTaskIsLocalPerWorktree(t *testing.T) {
	isolateEngineUserCache(t)
	firstWorktree := t.TempDir()
	secondWorktree := t.TempDir()
	for _, worktree := range []string{firstWorktree, secondWorktree} {
		if err := os.WriteFile(filepath.Join(worktree, "package-lock.json"), []byte("same-lockfile"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := &stampedInstallProject{}
	firstEngine, err := New(p, firstWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firstEngine.Run(context.Background(), Request{Target: "up", Worktree: firstWorktree, Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	if got := p.runs.Load(); got != 1 {
		t.Fatalf("runs after first worktree = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(secondWorktree, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("second worktree should not receive local install output from first worktree, stat err=%v", err)
	}

	secondEngine, err := New(p, secondWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondEngine.Run(context.Background(), Request{Target: "up", Worktree: secondWorktree, Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	if got := p.runs.Load(); got != 2 {
		t.Fatalf("runs after second worktree = %d, want 2", got)
	}
	if _, err := os.Stat(filepath.Join(secondWorktree, "node_modules", ".installed")); err != nil {
		t.Fatalf("second worktree should perform its own local install: %v", err)
	}
}

func TestStampedTaskDoesNotUseGlobalCacheToSkipOrRestore(t *testing.T) {
	isolateEngineUserCache(t)
	cacheSource := t.TempDir()
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "package-lock.json"), []byte("same-lockfile"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &stampedInstallProject{}
	task := p.Tasks()[0]
	rt := &project.Runtime{Worktree: worktree, Env: map[string]string{}}
	inputHashes, envValues, custom, err := fingerprint.CollectTaskInputs(context.Background(), worktree, task, rt)
	if err != nil {
		t.Fatal(err)
	}
	key, err := fingerprint.TaskKey(fingerprint.TaskKeyInput{
		Task:               task,
		InputHashes:        inputHashes,
		EnvValues:          envValues,
		CustomFingerprints: custom,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cacheSource, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheSource, "node_modules", ".installed"), []byte("from-global-cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := cache.NewNamespaced(instance.CacheRoot(), project.CacheNamespace(p))
	if _, err := store.Snapshot(cacheSource, task, key); err != nil {
		t.Fatal(err)
	}

	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background(), Request{Target: "up", Worktree: worktree, Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	if got := p.runs.Load(); got != 1 {
		t.Fatalf("stamped task should execute locally despite matching global cache entry; runs=%d", got)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "node_modules", ".installed"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "from-global-cache" {
		t.Fatal("stamped task restored node_modules from global cache")
	}
}

func TestWatchStampedTaskDoesNotLoopWhenCommandTouchesInputMtime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "package-lock.json"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &stampedInstallProject{touchInput: true}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- eng.Watch(ctx, Request{Target: "up", Worktree: worktree, Mode: api.ModeWatch})
	}()
	waitForEngineWatchReady(t, worktree)
	if got := p.runs.Load(); got != 1 {
		t.Fatalf("initial watch runs = %d, want 1", got)
	}
	if err := os.WriteFile(filepath.Join(worktree, "package-lock.json"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return p.runs.Load() == 2
	})
	if !waitForBool(2*time.Second, func() bool {
		return p.runs.Load() > 2
	}) {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("watch did not stop")
		}
		return
	}
	t.Fatalf("stamped task reran after touching its own unchanged input; runs=%d", p.runs.Load())
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

func waitForEngineWatchReady(t *testing.T, worktree string) string {
	return waitForEngineWatchReadyWithin(t, worktree, 4*time.Second)
}

func waitForEngineWatchReadyWithin(t *testing.T, worktree string, timeout time.Duration) string {
	t.Helper()
	instanceID, realWorktree, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, timeout, func() bool {
		_, err := os.Stat(instance.FlushWatchReadyPath(realWorktree, instanceID))
		return err == nil
	})
	return instanceID
}

func writeEngineFlushRequest(t *testing.T, worktree, instanceID string) string {
	t.Helper()
	requestID := fmt.Sprintf("test-flush-%d", time.Now().UnixNano())
	req := api.FlushRequest{
		ID:        requestID,
		CreatedAt: time.Now().UTC(),
		SyncPath:  instance.FlushSyncPath(worktree, instanceID, requestID),
	}
	if err := instance.WriteFlushRequest(worktree, instanceID, req); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(req.SyncPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(req.SyncPath, []byte(requestID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return requestID
}

func waitForEngineFlushAck(t *testing.T, worktree, instanceID, requestID string) api.FlushResult {
	t.Helper()
	var result api.FlushResult
	waitFor(t, 4*time.Second, func() bool {
		ack, err := instance.LoadFlushAck(worktree, instanceID, requestID)
		if err != nil {
			return false
		}
		result = ack
		return true
	})
	return result
}

func stringSliceHas(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
