package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

type watchRecoveryProject struct {
	tasks []project.Task
	roots []string
}

func (watchRecoveryProject) Name() string { return "watch-recovery" }
func (watchRecoveryProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{}, nil
}
func (p watchRecoveryProject) Tasks() []project.Task { return p.tasks }
func (p watchRecoveryProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: p.roots}}
}

func TestWatchRecoversCanceledPrerequisitesOutsideChangedBranch(t *testing.T) {
	for _, importFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("import_fails=%t", importFails), func(t *testing.T) {
			isolateEngineUserCache(t)
			root := t.TempDir()
			for path, content := range map[string]string{
				"schema.txt": "reverted schema", "migrations/history.txt": "mismatched history", "payload.txt": "unchanged",
			} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			synctest.Test(t, func(t *testing.T) {
				releaseSchema := make(chan struct{})
				releaseTypes := make(chan struct{})
				schemaEntered := make(chan struct{})
				typesEntered := make(chan struct{})
				counts := map[string]*atomic.Int32{}
				countRun := func(_ context.Context, rt *project.Runtime) error {
					counts[rt.TaskName].Add(1)
					return nil
				}
				startService := func(_ context.Context, rt *project.Runtime) error {
					counts[rt.TaskName].Add(1)
					rt.RegisterServiceHandle(newGenericServiceHandle())
					return nil
				}
				tasks := []project.Task{
					{Name: "node_install", Kind: project.KindOnce, Run: countRun},
					{Name: "prisma_client", Kind: project.KindOnce, Deps: []string{"node_install"}, Inputs: project.Inputs{Files: []string{"schema.txt"}}, Run: countRun},
					{
						Name: "prisma_schema_check", Kind: project.KindOnce, Deps: []string{"node_install"},
						Inputs: project.Inputs{Files: []string{"schema.txt"}, Dirs: []string{"migrations"}},
						Run: func(ctx context.Context, rt *project.Runtime) error {
							if counts[rt.TaskName].Add(1) == 1 {
								close(schemaEntered)
								select {
								case <-releaseSchema:
								case <-ctx.Done():
									return ctx.Err()
								}
							}
							data, err := os.ReadFile(rt.Abs("migrations/history.txt"))
							if err != nil {
								return err
							}
							if string(data) != "repaired" {
								return testMigrationNeededError{message: "schema and migration history disagree"}
							}
							return nil
						},
					},
					{
						Name: "prisma_migrations", Kind: project.KindOnce, Deps: []string{"node_install", "prisma_schema_check"},
						Inputs: project.Inputs{Files: []string{"schema.txt"}, Dirs: []string{"migrations"}}, Run: countRun,
					},
					{
						Name: "payload_types", Kind: project.KindOnce, Deps: []string{"node_install", "prisma_client"},
						Inputs: project.Inputs{Files: []string{"payload.txt"}},
						Run: func(ctx context.Context, rt *project.Runtime) error {
							if counts[rt.TaskName].Add(1) == 1 {
								close(typesEntered)
								select {
								case <-releaseTypes:
								case <-ctx.Done():
									return ctx.Err()
								}
							}
							return nil
						},
					},
					{
						Name: "payload_import_map", Kind: project.KindOnce, Deps: []string{"payload_types"},
						Inputs: project.Inputs{Files: []string{"payload.txt"}},
						Run: func(_ context.Context, rt *project.Runtime) error {
							counts[rt.TaskName].Add(1)
							if importFails {
								return errors.New("import map still fails")
							}
							return nil
						},
					},
					{Name: "payload_migrations", Kind: project.KindOnce, Deps: []string{"payload_import_map", "prisma_migrations"}, Run: countRun},
					{Name: "local_database", Kind: project.KindGroup, Deps: []string{"payload_migrations"}},
					{Name: "frontend_generate", Kind: project.KindGroup, Deps: []string{"prisma_client", "payload_import_map", "frontend_backend_client", "frontend_direct_backend_client", "featureflagsToTs"}},
					{Name: "frontend_dev", Kind: project.KindService, Deps: []string{"frontend_generate", "local_database"}, Restart: project.RestartOnInputChange, Run: startService},
					{Name: "backend_debug", Kind: project.KindDebugService, Deps: []string{"backend_docs", "sqlc", "local_database", "go_dependencies"}, Restart: project.RestartOnInputChange, Run: startService},
				}
				for _, name := range []string{"frontend_backend_client", "frontend_direct_backend_client", "featureflagsToTs", "backend_docs", "sqlc", "go_dependencies"} {
					tasks = append(tasks, project.Task{Name: name, Kind: project.KindOnce, Run: countRun})
				}
				for _, task := range tasks {
					counts[task.Name] = &atomic.Int32{}
				}
				eng, err := New(watchRecoveryProject{tasks: tasks, roots: []string{"backend_debug", "frontend_dev"}}, root)
				if err != nil {
					t.Fatal(err)
				}
				// Retain every state transition, including group completion (groups
				// have no callback), to verify ordering as well as the final labels.
				events := eng.SubscribeEventsLossless()
				var observed []api.Event
				stopEvents := make(chan struct{})
				go func() {
					for {
						select {
						case evt := <-events:
							observed = append(observed, evt)
						case <-stopEvents:
							return
						}
					}
				}()
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() {
					done <- eng.Watch(ctx, Request{Target: "dev", Worktree: root, Mode: api.ModeWatch, MaxParallel: 8})
				}()
				defer func() {
					cancel()
					if err := <-done; err != nil {
						t.Errorf("watch cleanup: %v", err)
					}
					close(stopEvents)
				}()
				<-schemaEntered
				<-typesEntered
				close(releaseSchema)
				// The scheduler consumes the failure and blocks on the only
				// remaining callback before types completes. No sleep/race decides
				// whether import-map generation starts before the failure.
				synctest.Wait()
				close(releaseTypes)
				synctest.Wait()
				id := waitForEngineWatchReady(t, root)
				initial, err := instance.LoadStatus(root, id)
				if err != nil {
					t.Fatal(err)
				}
				for name, want := range map[string]api.NodeState{
					"prisma_schema_check": api.StateMigrationNeeded, "prisma_client": api.StateDone, "payload_types": api.StateDone,
					"payload_import_map": api.StateCanceled, "frontend_generate": api.StateCanceled,
				} {
					if got := initial.Nodes[name].State; got != want {
						t.Fatalf("initial %s state = %s, want %s", name, got, want)
					}
				}
				runID := initial.Nodes["prisma_client"].RunID
				if err := os.WriteFile(filepath.Join(root, "migrations", "history.txt"), []byte("repaired"), 0o600); err != nil {
					t.Fatal(err)
				}
				result := flushWatchPolicyFreshnessTest(t, root, id)
				synctest.Wait()
				if !result.Synced || result.Success == importFails || result.RunID != runID {
					t.Errorf("recovery flush: run=%s want=%s synced=%t success=%t issues=%+v", result.RunID, runID, result.Synced, result.Success, result.Issues)
				}
				status, err := instance.LoadStatus(root, id)
				if err != nil {
					t.Fatal(err)
				}
				if got := counts["payload_import_map"].Load(); got != 1 {
					t.Errorf("required canceled import map ran %d times, want 1", got)
				}
				for _, name := range []string{"node_install", "prisma_client", "payload_types", "frontend_backend_client", "frontend_direct_backend_client", "featureflagsToTs", "backend_docs", "sqlc", "go_dependencies"} {
					if got := counts[name].Load(); got != 1 || status.Nodes[name].AttemptID != initial.Nodes[name].AttemptID {
						t.Errorf("satisfied external prerequisite %s reran: count=%d", name, got)
					}
				}
				for _, node := range result.Nodes {
					if node.State != status.Nodes[node.Name].State || node.RunID != runID {
						t.Errorf("flush and retained state disagree: %+v / %+v", node, status.Nodes[node.Name])
					}
				}
				for _, evt := range observed {
					if (evt.Type == api.EventTaskState || evt.Type == api.EventWatchCycleStart || evt.Type == api.EventWatchCycleDone) && evt.RunID != runID {
						t.Errorf("recovery event lost watch identity: type=%s task=%s run=%s want=%s", evt.Type, evt.Task, evt.RunID, runID)
					}
				}
				if importFails {
					if status.Nodes["payload_import_map"].State != api.StateFailed {
						t.Errorf("recovered prerequisite did not retain its failure: %+v", status.Nodes["payload_import_map"])
					}
					for _, name := range []string{"payload_migrations", "frontend_dev", "backend_debug"} {
						if counts[name].Load() != 0 || status.Nodes[name].State != api.StateBlocked {
							t.Errorf("consumer %s bypassed failed prerequisite: count=%d state=%s", name, counts[name].Load(), status.Nodes[name].State)
						}
					}
					// A sync-only flush must report the failure without retrying it.
					again := flushWatchPolicyFreshnessTest(t, root, id)
					if again.Success || counts["payload_import_map"].Load() != 1 {
						t.Errorf("flush retried unresolved prerequisite: success=%t count=%d", again.Success, counts["payload_import_map"].Load())
					}
					return
				}
				for _, name := range []string{"payload_import_map", "payload_migrations", "frontend_generate", "local_database"} {
					if status.Nodes[name].State != api.StateDone {
						t.Errorf("recovered %s state = %s, want done", name, status.Nodes[name].State)
					}
				}
				for _, name := range []string{"frontend_dev", "backend_debug"} {
					if node := status.Nodes[name]; node.State != api.StateRunning || !node.Ready || counts[name].Load() != 1 {
						t.Errorf("service %s did not recover once in the same watch: %+v", name, node)
					}
				}
				for _, pair := range [][2]string{{"payload_import_map", "payload_migrations"}, {"frontend_generate", "frontend_dev"}} {
					completed := false
					started := false
					for _, evt := range observed {
						if evt.Type != api.EventTaskState {
							continue
						}
						if evt.Task == pair[0] && evt.State == api.StateDone {
							completed = true
						}
						if evt.Task == pair[1] && (evt.State == api.StateStarting || evt.State == api.StateRunning) && !started {
							started = true
							if !completed {
								t.Errorf("%s started before %s completed", pair[1], pair[0])
							}
						}
					}
					if !completed || !started {
						t.Errorf("missing recovery events for %v: completed=%t started=%t", pair, completed, started)
					}
				}
			})
		})
	}
}

func TestWatchRecoveryRecursesThroughUnfinishedPrerequisites(t *testing.T) {
	isolateEngineUserCache(t)
	root := t.TempDir()
	input := filepath.Join(root, "check.txt")
	if err := os.WriteFile(input, []byte("fail"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	p := watchRecoveryProject{
		roots: []string{"consumer"},
		tasks: []project.Task{
			{Name: "a_check", Kind: project.KindOnce, Inputs: project.Inputs{Files: []string{"check.txt"}}, Run: func(_ context.Context, rt *project.Runtime) error {
				data, err := os.ReadFile(rt.Abs("check.txt"))
				if err != nil {
					return err
				}
				if string(data) != "repaired" {
					return errors.New("check failed")
				}
				return nil
			}},
			{Name: "z_first", Kind: project.KindOnce, Run: func(context.Context, *project.Runtime) error { calls = append(calls, "first"); return nil }},
			{Name: "z_second", Kind: project.KindOnce, Deps: []string{"z_first"}, Run: func(context.Context, *project.Runtime) error { calls = append(calls, "second"); return nil }},
			{Name: "z_group", Kind: project.KindGroup, Deps: []string{"z_second"}},
			{Name: "consumer", Kind: project.KindOnce, Deps: []string{"a_check", "z_group"}, Run: func(context.Context, *project.Runtime) error { calls = append(calls, "consumer"); return nil }},
		},
	}
	eng, err := New(p, root)
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- eng.Watch(ctx, Request{Target: "dev", Worktree: root, Mode: api.ModeWatch, MaxParallel: 1})
		}()
		defer func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("watch cleanup: %v", err)
			}
		}()
		id := waitForEngineWatchReady(t, root)
		synctest.Wait()
		if len(calls) != 0 {
			t.Fatalf("fixture executed prerequisites before the first failure: %v", calls)
		}
		if err := os.WriteFile(input, []byte("repaired"), 0o600); err != nil {
			t.Fatal(err)
		}
		result := flushWatchPolicyFreshnessTest(t, root, id)
		synctest.Wait()
		if !result.Success || fmt.Sprint(calls) != "[first second consumer]" {
			t.Fatalf("recursive recovery: success=%t calls=%v issues=%+v", result.Success, calls, result.Issues)
		}
	})
}

func TestWatchRecoveryDoesNotReuseWorkCanceledAfterEarlierSuccess(t *testing.T) {
	isolateEngineUserCache(t)
	root := t.TempDir()
	for _, name := range []string{"schema.txt", "migration.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("initial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var generationRuns atomic.Int32
	p := watchRecoveryProject{roots: []string{"consumer"}, tasks: []project.Task{
		{Name: "a_check", Kind: project.KindOnce, Inputs: project.Inputs{Files: []string{"schema.txt", "migration.txt"}}, Run: func(_ context.Context, rt *project.Runtime) error {
			schema, err := os.ReadFile(rt.Abs("schema.txt"))
			if err != nil {
				return err
			}
			migration, err := os.ReadFile(rt.Abs("migration.txt"))
			if err != nil {
				return err
			}
			if string(schema) != string(migration) {
				return testMigrationNeededError{message: "schema and migration disagree"}
			}
			return nil
		}},
		{Name: "z_generate", Kind: project.KindOnce, Inputs: project.Inputs{Files: []string{"schema.txt"}}, Run: func(context.Context, *project.Runtime) error { generationRuns.Add(1); return nil }},
		{Name: "z_group", Kind: project.KindGroup, Deps: []string{"z_generate"}},
		{Name: "consumer", Kind: project.KindService, Deps: []string{"a_check", "z_group"}, Run: func(_ context.Context, rt *project.Runtime) error {
			rt.RegisterServiceHandle(newGenericServiceHandle())
			return nil
		}},
	}}
	eng, err := New(p, root)
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- eng.Watch(ctx, Request{Target: "dev", Worktree: root, Mode: api.ModeWatch, MaxParallel: 1})
		}()
		defer func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("watch cleanup: %v", err)
			}
		}()
		id := waitForEngineWatchReady(t, root)
		initial, err := instance.LoadStatus(root, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "schema.txt"), []byte("changed schema"), 0o600); err != nil {
			t.Fatal(err)
		}
		failed := flushWatchPolicyFreshnessTest(t, root, id)
		if failed.Success {
			t.Fatal("mismatched schema did not fail")
		}
		status, err := instance.LoadStatus(root, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"z_generate", "z_group"} {
			if status.Nodes[name].State != api.StateCanceled {
				t.Errorf("unexecuted %s retained stale success after failed cycle: %+v", name, status.Nodes[name])
			}
		}
		record, err := instance.LoadRun(root, id, failed.RunID)
		if err != nil {
			t.Fatal(err)
		}
		for _, attempt := range record.Attempts {
			if attempt.AttemptID == initial.Nodes["z_generate"].AttemptID && (attempt.State != api.StateDone || !attempt.LogsComplete) {
				t.Errorf("cancellation rewrote the previous completed attempt: %+v", attempt)
			}
		}
		if err := os.WriteFile(filepath.Join(root, "migration.txt"), []byte("changed schema"), 0o600); err != nil {
			t.Fatal(err)
		}
		recovered := flushWatchPolicyFreshnessTest(t, root, id)
		if !recovered.Success || recovered.RunID != failed.RunID || generationRuns.Load() != 2 {
			t.Errorf("recovery reused canceled work: success=%t run=%s want=%s generation runs=%d issues=%+v", recovered.Success, recovered.RunID, failed.RunID, generationRuns.Load(), recovered.Issues)
		}
	})
}

func TestWatchRecoveryHonorsExternalPrerequisiteStateAndPolicy(t *testing.T) {
	for _, scenario := range []string{"cached", "healthy_service", "manually_stopped", "restart_never", "disallowed_warmup", "stale_warmup", "failed_service"} {
		t.Run(scenario, func(t *testing.T) {
			isolateEngineUserCache(t)
			root := t.TempDir()
			input := filepath.Join(root, "consumer.txt")
			if err := os.WriteFile(input, []byte("initial"), 0o600); err != nil {
				t.Fatal(err)
			}
			prerequisiteInput := filepath.Join(root, "prerequisite.txt")
			if err := os.WriteFile(prerequisiteInput, []byte("initial"), 0o600); err != nil {
				t.Fatal(err)
			}
			var prerequisiteRuns, consumerRuns atomic.Int32
			initialFailure := scenario == "restart_never" || scenario == "disallowed_warmup" || scenario == "failed_service"
			blocked := scenario == "manually_stopped" || scenario == "restart_never" || scenario == "disallowed_warmup" || scenario == "stale_warmup"
			prerequisite := project.Task{
				Name: "prerequisite", Kind: project.KindService, Restart: project.RestartOnInputChange,
				Run: func(_ context.Context, rt *project.Runtime) error {
					attempt := prerequisiteRuns.Add(1)
					if initialFailure && attempt == 1 {
						return errors.New("prerequisite initially fails")
					}
					rt.RegisterServiceHandle(newGenericServiceHandle())
					return nil
				},
			}
			switch scenario {
			case "cached":
				prerequisite.Kind = project.KindOnce
				prerequisite.Cache = true
				prerequisite.Outputs = project.Outputs{Files: []string{"artifact.txt"}}
				prerequisite.Run = func(_ context.Context, rt *project.Runtime) error {
					prerequisiteRuns.Add(1)
					return os.WriteFile(rt.Abs("artifact.txt"), []byte("cached artifact"), 0o600)
				}
			case "restart_never":
				prerequisite.Restart = project.RestartNever
			case "disallowed_warmup":
				prerequisite.Kind = project.KindWarmup
			case "stale_warmup":
				prerequisite.Kind = project.KindWarmup
				prerequisite.Inputs = project.Inputs{Files: []string{"prerequisite.txt"}}
				prerequisite.Run = func(context.Context, *project.Runtime) error { prerequisiteRuns.Add(1); return nil }
			}
			p := watchRecoveryProject{roots: []string{"consumer"}, tasks: []project.Task{
				prerequisite,
				{Name: "consumer", Kind: project.KindOnce, Deps: []string{"prerequisite"}, Inputs: project.Inputs{Files: []string{"consumer.txt"}}, Run: func(context.Context, *project.Runtime) error {
					consumerRuns.Add(1)
					return nil
				}},
			}}
			eng, err := New(p, root)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "cached" {
				if _, err := eng.Run(context.Background(), Request{Target: "dev", Worktree: root, Mode: api.ModeCI}); err != nil {
					t.Fatal(err)
				}
				consumerRuns.Store(0)
			}
			synctest.Test(t, func(t *testing.T) {
				controller := NewLifecycleController()
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() {
					done <- eng.Watch(ctx, Request{Target: "dev", Worktree: root, Mode: api.ModeWatch, LifecycleController: controller})
				}()
				defer func() {
					cancel()
					if err := <-done; err != nil {
						t.Errorf("watch cleanup: %v", err)
					}
				}()
				id := waitForEngineWatchReady(t, root)
				initial, err := instance.LoadStatus(root, id)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "cached" && initial.Nodes["prerequisite"].State != api.StateCached {
					t.Fatalf("fixture did not restore the cached prerequisite: %+v", initial.Nodes["prerequisite"])
				}
				if scenario == "manually_stopped" {
					if _, err := controller.Stop(ctx, "prerequisite"); err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
				}
				if scenario == "stale_warmup" {
					if err := os.WriteFile(prerequisiteInput, []byte("changed but watch-disallowed"), 0o600); err != nil {
						t.Fatal(err)
					}
					if result := flushWatchPolicyFreshnessTest(t, root, id); result.Success {
						t.Fatal("fixture did not block changed warmup")
					}
				}
				baseline := consumerRuns.Load()
				if err := os.WriteFile(input, []byte("changed consumer input only"), 0o600); err != nil {
					t.Fatal(err)
				}
				result := flushWatchPolicyFreshnessTest(t, root, id)
				status, err := instance.LoadStatus(root, id)
				if err != nil {
					t.Fatal(err)
				}
				if result.Success == blocked {
					t.Errorf("recovery success=%t blocked=%t issues=%+v", result.Success, blocked, result.Issues)
				}
				wantConsumer := baseline + 1
				if blocked {
					wantConsumer = baseline
					if status.Nodes["consumer"].State != api.StateBlocked {
						t.Errorf("consumer with unsatisfied prerequisite is not blocked: %+v", status.Nodes["consumer"])
					}
				}
				if got := consumerRuns.Load(); got != wantConsumer {
					t.Errorf("consumer runs=%d want=%d", got, wantConsumer)
				}
				wantPrerequisite := int32(1)
				if scenario == "failed_service" {
					wantPrerequisite = 2
				} else if status.Nodes["prerequisite"].AttemptID != initial.Nodes["prerequisite"].AttemptID {
					t.Error("recovery replaced a reusable or policy-blocked prerequisite attempt")
				}
				if got := prerequisiteRuns.Load(); got != wantPrerequisite {
					t.Errorf("prerequisite runs=%d want=%d", got, wantPrerequisite)
				}
				if scenario == "manually_stopped" && status.Nodes["prerequisite"].State != api.StateStopped {
					t.Error("recovery resurrected a manually stopped service")
				}
				if scenario == "manually_stopped" {
					if result, err := controller.Restart(ctx, "prerequisite"); err != nil || !result.Ready {
						t.Fatalf("explicit prerequisite restart failed: result=%+v error=%v", result, err)
					}
					// Lifecycle replies precede their observer reconciliation. Edit
					// only after it settles, so this test observes one complete write.
					synctest.Wait()
					if err := os.WriteFile(input, []byte("consumer edit after explicit restart"), 0o600); err != nil {
						t.Fatal(err)
					}
					result := flushWatchPolicyFreshnessTest(t, root, id)
					if !result.Success || prerequisiteRuns.Load() != 2 || consumerRuns.Load() != baseline+1 {
						t.Errorf("consumer did not reuse explicitly restarted prerequisite: success=%t prerequisite=%d consumer=%d", result.Success, prerequisiteRuns.Load(), consumerRuns.Load())
					}
				}
			})
		})
	}
}
