package gonextmonorepo

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/cli"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/engine"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
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

	if !waitForStableTraceCounts(8*time.Second, 500*time.Millisecond, worktree, map[string]int{
		"prisma_migrate":   1,
		"postgres":         1,
		"backend_dev":      1,
		"frontend_dev":     1,
		"frontend_codegen": 1,
	}) {
		t.Fatalf("initial watch run did not settle: %s", traceSnapshot(worktree, "prisma_migrate", "postgres", "backend_dev", "frontend_dev", "frontend_codegen"))
	}

	rewriteFile(t, filepath.Join(worktree, "db/migrations/001_init.sql"), "CREATE TABLE widgets(id INTEGER PRIMARY KEY, name TEXT NOT NULL, slug TEXT);\n")
	if !waitForStableTraceCounts(8*time.Second, 500*time.Millisecond, worktree, map[string]int{
		"prisma_migrate":   2,
		"postgres":         2,
		"backend_dev":      2,
		"frontend_dev":     1,
		"frontend_codegen": 1,
	}) {
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

func TestExampleProjectFlushSettlesWatchChange(t *testing.T) {
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

	instanceID := waitForExampleWatchReady(t, worktree)
	inst, err := instance.Load(worktree, instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.RecordDetachedRun(inst, api.RunConfig{
		Project:     "go-next-monorepo",
		Target:      "fullstack",
		Mode:        api.ModeWatch,
		MaxParallel: 4,
		Detached:    true,
	}, os.Getpid(), filepath.Join(worktree, ".devflow", "logs", instanceID, "supervisor.log")); err != nil {
		t.Fatal(err)
	}
	if !waitForStableTraceCounts(8*time.Second, 500*time.Millisecond, worktree, map[string]int{
		"postgres":         1,
		"backend_dev":      1,
		"frontend_dev":     1,
		"frontend_codegen": 1,
	}) {
		t.Fatalf("initial watch run did not settle: %s", traceSnapshot(worktree, "postgres", "backend_dev", "frontend_dev", "frontend_codegen"))
	}

	rewriteFile(t, filepath.Join(worktree, "frontend/src/page.tsx"), "export default function Page(){ return 'flush changed'; }\n")
	stdout := &bytes.Buffer{}
	app := &cli.App{Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"flush", "fullstack", "--json", "--project", "go-next-monorepo", "--worktree", worktree, "--timeout", "8s"}); err != nil {
		t.Fatalf("flush failed: %v\n%s", err, stdout.String())
	}
	var result api.FlushResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode flush result: %v\n%s", err, stdout.String())
	}
	if !result.Success || !result.Synced {
		t.Fatalf("expected successful flush, got %+v", result)
	}
	if result.Started {
		t.Fatalf("expected existing watch supervisor to be reused, got started=true in %+v", result)
	}
	if len(result.Services) < 2 {
		t.Fatalf("expected service health in flush result, got %+v", result.Services)
	}
	if got := traceCount(worktree, "frontend_dev"); got != 2 {
		t.Fatalf("expected frontend_dev rerun before flush ack, got %d", got)
	}
	if got := traceCount(worktree, "backend_dev"); got != 1 {
		t.Fatalf("unexpected backend_dev rerun for frontend-only change: %d", got)
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

func waitForExampleWatchReady(t *testing.T, worktree string) string {
	t.Helper()
	instanceID, realWorktree, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, func() bool {
		_, err := os.Stat(instance.FlushWatchReadyPath(realWorktree, instanceID))
		return err == nil
	})
	return instanceID
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
