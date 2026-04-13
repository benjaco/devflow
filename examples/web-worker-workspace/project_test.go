package webworkerworkspace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"devflow/pkg/api"
	"devflow/pkg/engine"
	"devflow/pkg/graph"
	"devflow/pkg/project"
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

	waitFor(t, 5*time.Second, func() bool {
		return traceCount(worktree, "db_migrate") == 1 &&
			traceCount(worktree, "postgres") == 1 &&
			traceCount(worktree, "contract_codegen") == 1 &&
			traceCount(worktree, "backend_codegen") == 1 &&
			traceCount(worktree, "frontend_codegen") == 1 &&
			traceCount(worktree, "worker_bundle") == 1 &&
			traceCount(worktree, "backend_dev") == 1 &&
			traceCount(worktree, "worker_dev") == 1 &&
			traceCount(worktree, "frontend_dev") == 1
	})
	time.Sleep(500 * time.Millisecond)

	rewriteFile(t, filepath.Join(worktree, "worker/src/worker.go"), "package worker\nvar Queue = \"priority\"\n")
	waitFor(t, 5*time.Second, func() bool {
		return traceCount(worktree, "worker_bundle") == 2 && traceCount(worktree, "worker_dev") == 2
	})
	if got := traceCount(worktree, "backend_dev"); got != 1 {
		t.Fatalf("unexpected backend restart after worker change: %d", got)
	}
	if got := traceCount(worktree, "frontend_dev"); got != 1 {
		t.Fatalf("unexpected frontend restart after worker change: %d", got)
	}

	rewriteFile(t, filepath.Join(worktree, "contracts/openapi.json"), "{ \"openapi\": \"3.0.0\", \"info\": { \"title\": \"workspace-api\", \"version\": \"2.0.0\" } }\n")
	if !waitForBool(5*time.Second, func() bool {
		return traceCount(worktree, "contract_codegen") == 2 &&
			traceCount(worktree, "backend_codegen") == 2 &&
			traceCount(worktree, "frontend_codegen") == 2 &&
			traceCount(worktree, "worker_bundle") == 3 &&
			traceCount(worktree, "backend_dev") == 2 &&
			traceCount(worktree, "worker_dev") == 3 &&
			traceCount(worktree, "frontend_dev") == 2
	}) {
		t.Fatalf("contract change did not rerun expected slice: contract=%d backend_codegen=%d frontend_codegen=%d worker_bundle=%d backend_dev=%d worker_dev=%d frontend_dev=%d", traceCount(worktree, "contract_codegen"), traceCount(worktree, "backend_codegen"), traceCount(worktree, "frontend_codegen"), traceCount(worktree, "worker_bundle"), traceCount(worktree, "backend_dev"), traceCount(worktree, "worker_dev"), traceCount(worktree, "frontend_dev"))
	}

	rewriteFile(t, filepath.Join(worktree, "db/migrations/001_init.sql"), "create table jobs(id integer primary key, payload text not null, status text not null);\n")
	if !waitForBool(5*time.Second, func() bool {
		return traceCount(worktree, "db_migrate") == 2 &&
			traceCount(worktree, "postgres") == 2 &&
			traceCount(worktree, "backend_dev") == 3 &&
			traceCount(worktree, "worker_dev") == 4
	}) {
		t.Fatalf("db change did not rerun expected slice: db_migrate=%d postgres=%d backend_dev=%d worker_dev=%d frontend_dev=%d", traceCount(worktree, "db_migrate"), traceCount(worktree, "postgres"), traceCount(worktree, "backend_dev"), traceCount(worktree, "worker_dev"), traceCount(worktree, "frontend_dev"))
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
	now := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, now, now); err != nil {
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
