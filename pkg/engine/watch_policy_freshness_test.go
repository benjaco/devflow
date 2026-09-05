package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

type watchPolicyFreshnessProject struct {
	tasks []project.Task
}

func (watchPolicyFreshnessProject) Name() string { return "watch-policy-freshness" }
func (watchPolicyFreshnessProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "watch-policy-freshness"}, nil
}
func (p watchPolicyFreshnessProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{p.tasks[len(p.tasks)-1].Name}}}
}
func (p watchPolicyFreshnessProject) Tasks() []project.Task { return p.tasks }

func TestWatchFlushReportsChangesBlockedByRestartPolicy(t *testing.T) {
	for _, policy := range []string{"restart_never_input", "restart_never_dependency", "blocked_warmup_dependency"} {
		t.Run(policy, func(t *testing.T) {
			root := t.TempDir()
			writeWatchFreshnessInput(t, root, "old")
			var sourceRuns, prepareRuns, serviceRuns atomic.Int32
			serve := project.Task{
				Name:    "serve",
				Kind:    project.KindService,
				Restart: project.RestartNever,
				Run: func(_ context.Context, rt *project.Runtime) error {
					serviceRuns.Add(1)
					rt.RegisterServiceHandle(newGenericServiceHandle())
					return nil
				},
			}
			var tasks []project.Task
			blockedTask := "serve"
			if policy == "restart_never_input" {
				serve.Inputs = project.Inputs{Files: []string{"input.txt"}}
			} else {
				tasks = append(tasks, project.Task{
					Name:   "source",
					Kind:   project.KindOnce,
					Inputs: project.Inputs{Files: []string{"input.txt"}},
					Run: func(context.Context, *project.Runtime) error {
						sourceRuns.Add(1)
						return nil
					},
				})
				serve.Deps = []string{"source"}
				if policy == "blocked_warmup_dependency" {
					blockedTask = "prepare"
					tasks = append(tasks, project.Task{
						Name:         "prepare",
						Kind:         project.KindWarmup,
						Deps:         []string{"source"},
						AllowInWatch: false,
						Run: func(context.Context, *project.Runtime) error {
							prepareRuns.Add(1)
							return nil
						},
					})
					serve.Deps = []string{"prepare"}
					serve.Restart = project.RestartOnInputChange
				}
			}
			tasks = append(tasks, serve)
			id := startWatchPolicyFreshnessTest(t, root, tasks)
			initial := flushWatchPolicyFreshnessTest(t, root, id)
			if !initial.Success {
				t.Fatalf("initial flush failed before any edits: %+v", initial)
			}

			writeWatchFreshnessInput(t, root, "updated source")
			for attempt := 0; attempt < 2; attempt++ {
				result := flushWatchPolicyFreshnessTest(t, root, id)
				if !result.Synced || result.Success {
					t.Errorf("flush %d must report unresolved changes without overriding restart policy: %+v", attempt+1, result)
				}
				found := false
				for _, issue := range result.Issues {
					if issue.Task == blockedTask && issue.Kind == "watch_restart_required" {
						found = true
					}
				}
				if !found {
					t.Errorf("flush %d lacks watch_restart_required for %s: %+v", attempt+1, blockedTask, result.Issues)
				}
			}
			if got := serviceRuns.Load(); got != 1 {
				t.Errorf("blocked service restarted: %d starts", got)
			}
			if policy != "restart_never_input" && sourceRuns.Load() != 2 {
				t.Errorf("unblocked source did not rebuild: %d runs", sourceRuns.Load())
			}
			if policy == "blocked_warmup_dependency" && prepareRuns.Load() != 1 {
				t.Errorf("disallowed warmup reran: %d runs", prepareRuns.Load())
			}
		})
	}
}

func TestWatchGeneratedOutputDoesNotHideSiblingSourceEdits(t *testing.T) {
	for _, declaration := range []string{"files", "paths"} {
		t.Run(declaration, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "shared"), 0o700); err != nil {
				t.Fatal(err)
			}
			sourcePath := filepath.Join(root, "shared", "source.txt")
			if err := os.WriteFile(sourcePath, []byte("initial"), 0o600); err != nil {
				t.Fatal(err)
			}
			outputs := project.Outputs{Files: []string{"shared/generated.txt"}}
			if declaration == "paths" {
				outputs = project.Outputs{Paths: []string{"shared/generated.txt"}}
			}
			var generateRuns, serviceRuns atomic.Int32
			tasks := []project.Task{
				{
					Name:    "generate",
					Kind:    project.KindOnce,
					Inputs:  project.Inputs{Files: []string{"shared/source.txt"}},
					Outputs: outputs,
					Run: func(_ context.Context, rt *project.Runtime) error {
						generateRuns.Add(1)
						data, err := os.ReadFile(rt.Abs("shared/source.txt"))
						if err != nil {
							return err
						}
						return os.WriteFile(rt.Abs("shared/generated.txt"), data, 0o600)
					},
				},
				{
					Name:    "serve",
					Kind:    project.KindService,
					Deps:    []string{"generate"},
					Inputs:  project.Inputs{Dirs: []string{"shared"}},
					Restart: project.RestartOnInputChange,
					Run: func(_ context.Context, rt *project.Runtime) error {
						serviceRuns.Add(1)
						rt.RegisterServiceHandle(newGenericServiceHandle())
						return nil
					},
				},
			}
			id := startWatchPolicyFreshnessTest(t, root, tasks)
			for index, content := range []string{"initial", "first source edit", "second sibling source edit"} {
				if index > 0 {
					if err := os.WriteFile(sourcePath, []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				for sync := 0; sync < 2; sync++ {
					result := flushWatchPolicyFreshnessTest(t, root, id)
					if !result.Success || !result.Synced {
						t.Errorf("flush after %q failed: %+v", content, result)
					}
				}
				data, err := os.ReadFile(filepath.Join(root, "shared", "generated.txt"))
				if err != nil || string(data) != content {
					t.Errorf("source edit was lost: artifact=%q error=%v want=%q", data, err, content)
				}
				wantRuns := int32(index + 1)
				if got := generateRuns.Load(); got != wantRuns {
					t.Errorf("generation after %q: got %d runs, want %d", content, got, wantRuns)
				}
				if got := serviceRuns.Load(); got != wantRuns {
					t.Errorf("service after %q: got %d starts, want %d; generated writes must not add restarts", content, got, wantRuns)
				}
			}
		})
	}
}

func TestWatchFlushRechecksInputsAfterReadinessProbe(t *testing.T) {
	root := t.TempDir()
	writeWatchFreshnessInput(t, root, "old")
	var generateRuns, serviceRuns, readyCalls atomic.Int32
	var latestReadyInput atomic.Value
	latestReadyInput.Store("")
	probeEntered := make(chan string, 1)
	resumeProbe := make(chan struct{})
	tasks := []project.Task{
		{
			Name:    "generate",
			Kind:    project.KindOnce,
			Inputs:  project.Inputs{Files: []string{"input.txt"}},
			Outputs: project.Outputs{Paths: []string{"out.txt"}},
			Cache:   true,
			Run: func(_ context.Context, rt *project.Runtime) error {
				generateRuns.Add(1)
				data, err := os.ReadFile(rt.Abs("input.txt"))
				if err != nil {
					return err
				}
				return os.WriteFile(rt.Abs("out.txt"), data, 0o600)
			},
		},
		{
			Name:         "serve",
			Kind:         project.KindService,
			Deps:         []string{"generate"},
			Restart:      project.RestartOnInputChange,
			ReadyTimeout: 5 * time.Second,
			Run: func(_ context.Context, rt *project.Runtime) error {
				serviceRuns.Add(1)
				rt.RegisterServiceHandle(newGenericServiceHandle())
				return nil
			},
			Ready: func(ctx context.Context, rt *project.Runtime) error {
				data, err := os.ReadFile(rt.Abs("out.txt"))
				if err != nil {
					return err
				}
				latestReadyInput.Store(string(data))
				// The first probe starts the service; the second belongs to flush.
				if readyCalls.Add(1) == 2 {
					probeEntered <- string(data)
					select {
					case <-resumeProbe:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			},
		},
	}
	id := startWatchPolicyFreshnessTest(t, root, tasks)
	if got := readyCalls.Load(); got != 1 {
		t.Fatalf("expected one startup readiness probe, got %d", got)
	}
	requestID := writeEngineFlushRequest(t, root, id)
	select {
	case got := <-probeEntered:
		if got != "old" {
			t.Fatalf("blocked flush probe saw %q, want old artifact", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("flush readiness probe did not reach its barrier")
	}
	writeWatchFreshnessInput(t, root, "updated during readiness probe")
	close(resumeProbe)

	result := waitForEngineFlushAck(t, root, id, requestID)
	assertWatchFreshnessResult(t, root, result, "updated during readiness probe")
	if got := generateRuns.Load(); got != 2 {
		t.Errorf("flush did not rebuild the changed input: %d generation attempts", got)
	}
	if got := serviceRuns.Load(); got != 2 {
		t.Errorf("flush did not restart the affected service: %d starts", got)
	}
	if readyCalls.Load() < 4 || latestReadyInput.Load() != "updated during readiness probe" {
		t.Errorf("flush did not recheck the updated service: %d probes, latest input %q", readyCalls.Load(), latestReadyInput.Load())
	}
	for _, node := range result.Nodes {
		if node.Name == "serve" && node.Generation != 2 {
			t.Errorf("flush refers to old service generation %d, want 2", node.Generation)
		}
	}
}

func TestWatchFlushPreservesInPlaceInputEditAfterFormatterCompletes(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("initial source"), 0o600); err != nil {
		t.Fatal(err)
	}
	var formatRuns, checkRuns atomic.Int32
	checkEntered := make(chan string, 1)
	resumeCheck := make(chan struct{})
	tasks := []project.Task{
		{
			Name:    "format",
			Kind:    project.KindOnce,
			Inputs:  project.Inputs{Files: []string{"source.txt"}},
			Outputs: project.Outputs{Paths: []string{"source.txt"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				formatRuns.Add(1)
				data, err := os.ReadFile(rt.Abs("source.txt"))
				if err != nil {
					return err
				}
				return os.WriteFile(rt.Abs("source.txt"), []byte(strings.ToUpper(string(data))), 0o600)
			},
		},
		{
			Name:    "check",
			Kind:    project.KindOnce,
			Deps:    []string{"format"},
			Inputs:  project.Inputs{Files: []string{"source.txt"}},
			Outputs: project.Outputs{Paths: []string{"checked.txt"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				attempt := checkRuns.Add(1)
				data, err := os.ReadFile(rt.Abs("source.txt"))
				if err != nil {
					return err
				}
				if attempt == 2 {
					checkEntered <- string(data)
					select {
					case <-resumeCheck:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return os.WriteFile(rt.Abs("checked.txt"), data, 0o600)
			},
		},
	}
	id := startWatchPolicyFreshnessTest(t, root, tasks)
	if err := os.WriteFile(sourcePath, []byte("first source edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-checkEntered:
		if got != "FIRST SOURCE EDIT" {
			t.Fatalf("downstream check read %q, want completed formatter output", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("downstream check did not reach its input-read barrier")
	}
	if err := os.WriteFile(sourcePath, []byte("newest edit after formatting"), 0o600); err != nil {
		t.Fatal(err)
	}
	requestID := writeEngineFlushRequest(t, root, id)
	close(resumeCheck)

	result := waitForEngineFlushAck(t, root, id, requestID)
	if !result.Synced || !result.Success {
		t.Errorf("flush failed after in-place source edit: %+v", result)
	}
	for _, path := range []string{"source.txt", "checked.txt"} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(data) != "NEWEST EDIT AFTER FORMATTING" {
			t.Errorf("flush lost the edit made after formatting: %s=%q error=%v", path, data, err)
		}
	}
	if got := formatRuns.Load(); got != 3 {
		t.Errorf("expected initial formatting and both user edits, got %d runs", got)
	}
	if got := checkRuns.Load(); got != 3 {
		t.Errorf("expected initial check and both user edits, got %d runs", got)
	}
}

func startWatchPolicyFreshnessTest(t *testing.T, root string, tasks []project.Task) string {
	t.Helper()
	isolateEngineUserCache(t)
	eng, err := New(watchPolicyFreshnessProject{tasks: tasks}, root)
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
	return waitForEngineWatchReady(t, root)
}

func flushWatchPolicyFreshnessTest(t *testing.T, root, instanceID string) api.FlushResult {
	t.Helper()
	requestID := writeEngineFlushRequest(t, root, instanceID)
	return waitForEngineFlushAck(t, root, instanceID, requestID)
}
