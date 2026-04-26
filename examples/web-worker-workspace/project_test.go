package webworkerworkspace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/engine"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/project"
)

func TestWorkspaceProjectRegistered(t *testing.T) {
	p, err := project.Lookup("web-worker-workspace")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Name(); got != "web-worker-workspace" {
		t.Fatalf("unexpected project name %q", got)
	}
}

func TestWorkspaceProjectDetectionAndDefaultTarget(t *testing.T) {
	worktree := seededWorktree(t)
	p, err := project.Detect(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Name(); got != "web-worker-workspace" {
		t.Fatalf("unexpected detected project %q", got)
	}
	if got := project.PreferredTarget(p); got != "fullstack" {
		t.Fatalf("unexpected default target %q", got)
	}
}

func TestWorkspaceGraphValidates(t *testing.T) {
	p := workspaceProject{}
	g, err := graph.New(p.Tasks(), p.Targets())
	if err != nil {
		t.Fatal(err)
	}
	closure, err := g.TargetClosure("fullstack")
	if err != nil {
		t.Fatal(err)
	}
	if len(closure) == 0 {
		t.Fatal("expected fullstack closure to be non-empty")
	}
	required := []string{
		"prepare_db_base",
		"db_migrate",
		"postgres",
		"contract_codegen",
		"backend_codegen",
		"frontend_codegen",
		"worker_bundle",
		"backend_dev",
		"worker_dev",
		"frontend_dev",
	}
	for _, name := range required {
		if _, ok := g.Tasks[name]; !ok {
			t.Fatalf("expected task %q to be registered", name)
		}
	}
}

func TestWorkspaceProjectCachesOnSecondRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DEVFLOW_WEBWORKER_FAKE_DB", "1")
	worktree := seededWorktree(t)

	eng, err := engine.New(workspaceProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	first, err := eng.Run(context.Background(), engine.Request{
		Target:      "fullstack",
		Worktree:    worktree,
		Mode:        api.ModeCI,
		MaxParallel: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Result.CacheHits) != 0 {
		t.Fatalf("unexpected first-run cache hits: %v", first.Result.CacheHits)
	}
	second, err := eng.Run(context.Background(), engine.Request{
		Target:      "fullstack",
		Worktree:    worktree,
		Mode:        api.ModeCI,
		MaxParallel: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Result.CacheHits) < 4 {
		t.Fatalf("expected substantial cache hits on second run, got %v", second.Result.CacheHits)
	}
	assertFileExists(t, filepath.Join(worktree, "generated/contracts/api.json"))
	assertFileExists(t, filepath.Join(worktree, "worker/generated/bundle.json"))
	prepare := readJSONMap(t, filepath.Join(worktree, ".devflow/web-worker-workspace/db/prepare.json"))
	if got, ok := prepare["exactMatch"].(bool); !ok || !got {
		t.Fatalf("expected second run to reuse exact DB snapshot, got %v", prepare)
	}
}

func TestWorkspaceProjectIsolatesTwoWorktrees(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DEVFLOW_WEBWORKER_FAKE_DB", "1")
	worktreeA := seededWorktree(t)
	worktreeB := seededWorktree(t)

	type result struct {
		out *engine.Outcome
		err error
	}
	run := func(worktree string, ch chan<- result) {
		eng, err := engine.New(workspaceProject{}, worktree)
		if err != nil {
			ch <- result{err: err}
			return
		}
		out, err := eng.Run(context.Background(), engine.Request{
			Target:      "fullstack",
			Worktree:    worktree,
			Mode:        api.ModeCI,
			MaxParallel: 4,
		})
		ch <- result{out: out, err: err}
	}

	ch := make(chan result, 2)
	go run(worktreeA, ch)
	go run(worktreeB, ch)
	first := <-ch
	second := <-ch
	if first.err != nil {
		t.Fatal(first.err)
	}
	if second.err != nil {
		t.Fatal(second.err)
	}
	if first.out.Instance.DB.Name == second.out.Instance.DB.Name {
		t.Fatalf("expected distinct DB names, got %q", first.out.Instance.DB.Name)
	}
	for _, portName := range []string{"backend", "frontend", "worker", "postgres"} {
		if first.out.Instance.Ports[portName] == second.out.Instance.Ports[portName] {
			t.Fatalf("expected distinct %s ports: %v vs %v", portName, first.out.Instance.Ports, second.out.Instance.Ports)
		}
	}
}

func TestWorkspaceWatchSelectiveReruns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DEVFLOW_WEBWORKER_FAKE_DB", "1")
	worktree := seededWorktree(t)
	eng, err := engine.New(workspaceProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEvents()
	var (
		eventMu     sync.Mutex
		watchStarts []string
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- eng.Watch(ctx, engine.Request{
			Target:      "fullstack",
			Worktree:    worktree,
			Mode:        api.ModeWatch,
			MaxParallel: 4,
		})
	}()
	go func() {
		for evt := range events {
			if evt.Type != api.EventWatchCycleStart {
				continue
			}
			eventMu.Lock()
			watchStarts = append(watchStarts, "files="+strings.Join(evt.Files, ",")+" affected="+strings.Join(evt.AffectedTasks, ","))
			if len(watchStarts) > 12 {
				watchStarts = append([]string(nil), watchStarts[len(watchStarts)-12:]...)
			}
			eventMu.Unlock()
		}
	}()

	if !waitForStableTraceCounts(8*time.Second, 500*time.Millisecond, worktree, map[string]int{
		"db_migrate":       1,
		"postgres":         1,
		"contract_codegen": 1,
		"backend_codegen":  1,
		"frontend_codegen": 1,
		"worker_bundle":    1,
		"backend_dev":      1,
		"worker_dev":       1,
		"frontend_dev":     1,
	}) {
		t.Fatalf("initial watch run did not settle: %s", traceSnapshot(worktree, "db_migrate", "postgres", "contract_codegen", "backend_codegen", "frontend_codegen", "worker_bundle", "backend_dev", "worker_dev", "frontend_dev"))
	}

	rewriteFile(t, filepath.Join(worktree, "worker/src/worker.go"), "package worker\nvar Queue = \"priority\"\n")
	if !waitForStableTraceCounts(8*time.Second, 500*time.Millisecond, worktree, map[string]int{
		"worker_bundle": 2,
		"worker_dev":    2,
		"backend_dev":   1,
		"frontend_dev":  1,
	}) {
		t.Fatalf("worker change did not settle on expected slice: %s", traceSnapshot(worktree, "worker_bundle", "worker_dev", "backend_dev", "frontend_dev"))
	}
	if got := traceCount(worktree, "backend_dev"); got != 1 {
		t.Fatalf("unexpected backend restart after worker change: %d", got)
	}
	if got := traceCount(worktree, "frontend_dev"); got != 1 {
		t.Fatalf("unexpected frontend restart after worker change: %d", got)
	}

	rewriteFile(t, filepath.Join(worktree, "contracts/openapi.json"), "{ \"openapi\": \"3.0.0\", \"info\": { \"title\": \"workspace-api\", \"version\": \"2.0.0\" } }\n")
	if !waitForStableTraceCounts(8*time.Second, 500*time.Millisecond, worktree, map[string]int{
		"contract_codegen": 2,
		"backend_codegen":  2,
		"frontend_codegen": 2,
		"worker_bundle":    3,
		"backend_dev":      2,
		"worker_dev":       3,
		"frontend_dev":     2,
	}) {
		t.Fatalf("contract change did not rerun expected slice: %s watch=%s", traceSnapshot(worktree, "contract_codegen", "backend_codegen", "frontend_codegen", "worker_bundle", "backend_dev", "worker_dev", "frontend_dev"), recentWatchStarts(&eventMu, watchStarts))
	}

	rewriteFile(t, filepath.Join(worktree, "db/migrations/001_init.sql"), "create table jobs(id integer primary key, payload text not null, status text not null);\n")
	if !waitForStableTraceCounts(8*time.Second, 500*time.Millisecond, worktree, map[string]int{
		"db_migrate":   2,
		"postgres":     2,
		"backend_dev":  3,
		"worker_dev":   4,
		"frontend_dev": 2,
	}) {
		t.Fatalf("db change did not rerun expected slice: %s watch=%s", traceSnapshot(worktree, "db_migrate", "postgres", "backend_dev", "worker_dev", "frontend_dev"), recentWatchStarts(&eventMu, watchStarts))
	}
	if got := traceCount(worktree, "frontend_dev"); got != 2 {
		t.Fatalf("unexpected frontend restart after DB change: %d", got)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("watch returned error: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for watch shutdown")
	}
}

func seededWorktree(t *testing.T) string {
	t.Helper()
	worktree := t.TempDir()
	if err := SeedWorktree(worktree); err != nil {
		t.Fatal(err)
	}
	return worktree
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

func waitForStableTraceCounts(timeout, stableFor time.Duration, worktree string, expected map[string]int) bool {
	deadline := time.Now().Add(timeout)
	var stableSince time.Time
	for time.Now().Before(deadline) {
		if traceCountsMatch(worktree, expected) {
			if stableSince.IsZero() {
				stableSince = time.Now()
			}
			if time.Since(stableSince) >= stableFor {
				return true
			}
		} else {
			stableSince = time.Time{}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func traceCountsMatch(worktree string, expected map[string]int) bool {
	for task, want := range expected {
		if traceCount(worktree, task) != want {
			return false
		}
	}
	return true
}

func traceSnapshot(worktree string, tasks ...string) string {
	parts := make([]string, 0, len(tasks))
	for _, task := range tasks {
		parts = append(parts, task+"="+strconv.Itoa(traceCount(worktree, task)))
	}
	return strings.Join(parts, " ")
}

func recentWatchStarts(mu *sync.Mutex, values []string) string {
	mu.Lock()
	defer mu.Unlock()
	if len(values) == 0 {
		return "<none>"
	}
	return strings.Join(append([]string(nil), values...), " | ")
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file %s to exist: %v", path, err)
	}
}

func rewriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}
