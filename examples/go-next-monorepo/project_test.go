package gonextmonorepo

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"devflow/pkg/api"
	"devflow/pkg/engine"
	"devflow/pkg/project"
)

func TestExampleProjectCachesOnSecondRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DEVFLOW_EXAMPLE_FAKE_DB", "1")
	worktree := seededWorktree(t)

	eng, err := engine.New(exampleProject{}, worktree)
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
	if len(second.Result.CacheHits) < 6 {
		t.Fatalf("expected substantial cache hits on second run, got %v", second.Result.CacheHits)
	}
	assertFileExists(t, filepath.Join(worktree, "backend/generated/openapi-external.json"))
	assertFileExists(t, filepath.Join(worktree, "frontend/generated/api-client.json"))
	assertFileExists(t, filepath.Join(worktree, ".devflow/example/db/migrate.json"))
	prepare := readJSONMap(t, filepath.Join(worktree, ".devflow/example/db/prepare.json"))
	if got, ok := prepare["exactMatch"].(bool); !ok || !got {
		t.Fatalf("expected second run to reuse exact DB snapshot, got %v", prepare)
	}
}

func TestExampleProjectDetectionAndDefaultTarget(t *testing.T) {
	worktree := seededWorktree(t)
	p, err := project.Detect(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Name(); got != "go-next-monorepo" {
		t.Fatalf("unexpected detected project %q", got)
	}
	if got := project.PreferredTarget(p); got != "fullstack" {
		t.Fatalf("unexpected default target %q", got)
	}
}

func TestExampleProjectIsolatesTwoWorktrees(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DEVFLOW_EXAMPLE_FAKE_DB", "1")
	worktreeA := seededWorktree(t)
	worktreeB := seededWorktree(t)

	type result struct {
		out *engine.Outcome
		err error
	}
	run := func(worktree string, ch chan<- result) {
		eng, err := engine.New(exampleProject{}, worktree)
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
	if first.out.Instance.Ports["backend"] == second.out.Instance.Ports["backend"] {
		t.Fatalf("expected distinct backend ports: %v vs %v", first.out.Instance.Ports, second.out.Instance.Ports)
	}
	if first.out.Instance.Ports["frontend"] == second.out.Instance.Ports["frontend"] {
		t.Fatalf("expected distinct frontend ports: %v vs %v", first.out.Instance.Ports, second.out.Instance.Ports)
	}
	if first.out.Instance.Ports["postgres"] == second.out.Instance.Ports["postgres"] {
		t.Fatalf("expected distinct postgres ports: %v vs %v", first.out.Instance.Ports, second.out.Instance.Ports)
	}
}

func TestExampleProjectWatchSelectiveReruns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DEVFLOW_EXAMPLE_FAKE_DB", "1")
	worktree := seededWorktree(t)
	eng, err := engine.New(exampleProject{}, worktree)
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
		return traceCount(worktree, "prisma_migrate") == 1 &&
			traceCount(worktree, "postgres") == 1 &&
			traceCount(worktree, "backend_dev") == 1 &&
			traceCount(worktree, "frontend_dev") == 1 &&
			traceCount(worktree, "frontend_codegen") == 1
	})
	time.Sleep(500 * time.Millisecond)

	rewriteFile(t, filepath.Join(worktree, "db/migrations/001_init.sql"), "CREATE TABLE widgets(id INTEGER PRIMARY KEY, name TEXT NOT NULL, slug TEXT);\n")
	ok := waitForBool(5*time.Second, func() bool {
		return traceCount(worktree, "prisma_migrate") == 2 && traceCount(worktree, "postgres") == 2 && traceCount(worktree, "backend_dev") == 2
	})
	if !ok {
		t.Fatalf("migration change did not rerun expected slice: prisma_migrate=%d postgres=%d backend_dev=%d frontend_dev=%d frontend_codegen=%d", traceCount(worktree, "prisma_migrate"), traceCount(worktree, "postgres"), traceCount(worktree, "backend_dev"), traceCount(worktree, "frontend_dev"), traceCount(worktree, "frontend_codegen"))
	}
	if got := traceCount(worktree, "frontend_dev"); got != 1 {
		t.Fatalf("unexpected frontend restart after migration change: %d", got)
	}
	if got := traceCount(worktree, "frontend_codegen"); got != 1 {
		t.Fatalf("unexpected frontend_codegen rerun after migration change: %d", got)
	}

	rewriteFile(t, filepath.Join(worktree, "frontend/src/page.tsx"), "export default function Page(){ return 'changed'; }\n")
	waitFor(t, 5*time.Second, func() bool {
		return traceCount(worktree, "frontend_dev") == 2
	})
	if got := traceCount(worktree, "backend_dev"); got != 2 {
		t.Fatalf("unexpected backend restart after frontend change: %d", got)
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

func TestExampleProjectWatchEmitsCycleForChangedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DEVFLOW_EXAMPLE_FAKE_DB", "1")
	worktree := seededWorktree(t)
	eng, err := engine.New(exampleProject{}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEvents()
	watchEvents := make(chan api.Event, 16)
	go func() {
		for evt := range events {
			if evt.Type == api.EventWatchCycleStart || evt.Type == api.EventWatchCycleDone {
				watchEvents <- evt
			}
		}
	}()

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
		return traceCount(worktree, "frontend_dev") == 1
	})
	time.Sleep(500 * time.Millisecond)

	changedRel := "frontend/src/page.tsx"
	changedTask := "frontend_dev"
	rewriteFile(t, filepath.Join(worktree, changedRel), "export default function Page(){ return 'watch-event'; }\n")

	var sawStart bool
	var sawDone bool
	if !waitForBool(5*time.Second, func() bool {
		for {
			select {
			case evt := <-watchEvents:
				if evt.Type == api.EventWatchCycleStart && stringSliceContains(evt.Files, changedTask) {
					sawStart = true
				}
				if evt.Type == api.EventWatchCycleDone && stringSliceContains(evt.Files, changedTask) && evt.Success != nil && *evt.Success {
					sawDone = true
				}
			default:
				return sawStart &&
					sawDone &&
					traceCount(worktree, "frontend_dev") == 2
			}
		}
	}) {
		t.Fatalf("watch change was not fully observed: sawStart=%v sawDone=%v frontend_dev=%d", sawStart, sawDone, traceCount(worktree, "frontend_dev"))
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

func stringSliceContains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
