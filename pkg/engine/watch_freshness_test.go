package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

type watchFreshnessProject struct {
	runs         atomic.Int32
	blockAttempt int32
	read         chan string
	resume       chan struct{}
	failOld      bool
}

func (*watchFreshnessProject) Name() string { return "watch-freshness" }

func (*watchFreshnessProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "watch-freshness"}, nil
}

func (*watchFreshnessProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"generate"}}}
}

func (p *watchFreshnessProject) Tasks() []project.Task {
	return []project.Task{{
		Name:    "generate",
		Kind:    project.KindOnce,
		Inputs:  project.Inputs{Files: []string{"input.txt"}},
		Outputs: project.Outputs{Files: []string{"out.txt"}},
		Cache:   true,
		Run: func(ctx context.Context, rt *project.Runtime) error {
			attempt := p.runs.Add(1)
			data, err := os.ReadFile(rt.Abs("input.txt"))
			if err != nil {
				return err
			}
			if attempt == p.blockAttempt {
				p.read <- string(data)
				select {
				case <-p.resume:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if p.failOld && string(data) == "old" {
				return fmt.Errorf("old input cannot be built")
			}
			return os.WriteFile(rt.Abs("out.txt"), data, 0o600)
		},
	}}
}

func TestWatchFlushIncludesChangesDuringInitialRun(t *testing.T) {
	for _, failInitial := range []bool{false, true} {
		name := "initial_success"
		if failInitial {
			name = "initial_failure"
		}
		t.Run(name, func(t *testing.T) {
			root, p := startWatchFreshnessTest(t, 1, failInitial)
			waitForWatchFreshnessRead(t, p, "old")
			writeWatchFreshnessInput(t, root, "updated during startup")
			close(p.resume)

			id := waitForEngineWatchReady(t, root)
			requestID := writeEngineFlushRequest(t, root, id)
			result := waitForEngineFlushAck(t, root, id, requestID)
			assertWatchFreshnessResult(t, root, result, "updated during startup")
			if got := p.runs.Load(); got != 2 {
				t.Errorf("expected initial attempt and updated-input attempt before flush, got %d runs", got)
			}
		})
	}
}

func TestWatchFlushIncludesChangesDuringRebuild(t *testing.T) {
	root, p := startWatchFreshnessTest(t, 2, false)
	id := waitForEngineWatchReady(t, root)
	writeWatchFreshnessInput(t, root, "first edit")
	requestID := writeEngineFlushRequest(t, root, id)
	waitForWatchFreshnessRead(t, p, "first edit")
	if _, err := instance.LoadFlushAck(root, id, requestID); !os.IsNotExist(err) {
		t.Fatalf("flush acknowledged while its rebuild was blocked: %v", err)
	}
	writeWatchFreshnessInput(t, root, "second edit during rebuild")
	close(p.resume)

	result := waitForEngineFlushAck(t, root, id, requestID)
	assertWatchFreshnessResult(t, root, result, "second edit during rebuild")
	if got := p.runs.Load(); got != 3 {
		t.Errorf("expected initial attempt and both edits before flush, got %d runs", got)
	}
}

func TestWatchConcurrentFlushesIncludeChangesDuringRebuild(t *testing.T) {
	root, p := startWatchFreshnessTest(t, 2, false)
	id := waitForEngineWatchReady(t, root)
	writeWatchFreshnessInput(t, root, "first edit")
	firstRequest := writeEngineFlushRequest(t, root, id)
	waitForWatchFreshnessRead(t, p, "first edit")

	writeWatchFreshnessInput(t, root, "newest edit during rebuild")
	secondRequest := writeEngineFlushRequest(t, root, id)
	requests := []string{firstRequest, secondRequest}
	for _, requestID := range requests {
		if _, err := instance.LoadFlushAck(root, id, requestID); !os.IsNotExist(err) {
			t.Fatalf("flush %s acknowledged while the rebuild was blocked: %v", requestID, err)
		}
	}
	close(p.resume)

	var runKey string
	for _, requestID := range requests {
		result := waitForEngineFlushAck(t, root, id, requestID)
		assertWatchFreshnessResult(t, root, result, "newest edit during rebuild")
		if result.RequestID != requestID {
			t.Errorf("flush result request ID = %q, want %q", result.RequestID, requestID)
		}
		if len(result.Nodes) != 1 || result.Nodes[0].LastRunKey == "" {
			t.Fatalf("flush lacks the completed artifact's cache key: %+v", result.Nodes)
		}
		if runKey != "" && result.Nodes[0].LastRunKey != runKey {
			t.Errorf("concurrent flushes reference different artifacts: %q and %q", runKey, result.Nodes[0].LastRunKey)
		}
		runKey = result.Nodes[0].LastRunKey
	}
	if got := p.runs.Load(); got != 3 {
		t.Errorf("expected initial attempt and both edits before either flush, got %d runs", got)
	}
}

func startWatchFreshnessTest(t *testing.T, blockAttempt int32, failOld bool) (string, *watchFreshnessProject) {
	t.Helper()
	cacheHome := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CACHE_HOME", "LOCALAPPDATA"} {
		t.Setenv(key, cacheHome)
	}
	root := t.TempDir()
	writeWatchFreshnessInput(t, root, "old")
	p := &watchFreshnessProject{
		blockAttempt: blockAttempt,
		read:         make(chan string, 1),
		resume:       make(chan struct{}),
		failOld:      failOld,
	}
	eng, err := New(p, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- eng.Watch(ctx, Request{Target: "dev", Worktree: root, Mode: api.ModeWatch})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("watch shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("watch did not exit after cancellation")
		}
	})
	return root, p
}

func waitForWatchFreshnessRead(t *testing.T, p *watchFreshnessProject, want string) {
	t.Helper()
	select {
	case got := <-p.read:
		if got != want {
			t.Fatalf("blocked task read %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("task did not reach its input-read barrier")
	}
}

func writeWatchFreshnessInput(t *testing.T, root, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertWatchFreshnessResult(t *testing.T, root string, result api.FlushResult, want string) {
	t.Helper()
	if !result.Success || !result.Synced {
		t.Errorf("expected a successful flush after rebuilding updated input, got %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(root, "out.txt"))
	if err != nil || string(data) != want {
		t.Errorf("flush acknowledged artifact %q (read error %v), want %q", data, err, want)
	}
}
