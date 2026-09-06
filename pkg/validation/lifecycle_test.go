package validation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

func TestValidationBeforeRunEnvironmentIsTaskLocal(t *testing.T) {
	for _, mode := range []api.ValidationMode{api.ValidationModeArtifacts, api.ValidationModeOrders} {
		t.Run(string(mode), func(t *testing.T) {
			calls := map[string]int{}
			p := validationTestProject{name: "hook-env", targets: []project.Target{{Name: "build", RootTasks: []string{"a", "b"}}}}
			for _, name := range []string{"a", "b"} {
				p.tasks = append(p.tasks, project.Task{
					Name: name, Kind: project.KindOnce,
					Outputs: project.Outputs{Files: []string{name + ".txt"}},
					BeforeRun: func(_ context.Context, rt *project.Runtime) error {
						if previous := rt.Env["HOOK_TASK"]; previous != "" {
							return fmt.Errorf("hook environment leaked from %s into %s", previous, name)
						}
						calls[name]++
						rt.Env["HOOK_TASK"] = name
						return nil
					},
					Run: func(_ context.Context, rt *project.Runtime) error {
						if rt.TaskName != name || rt.Env["HOOK_TASK"] != name {
							return fmt.Errorf("task %s did not receive its hook runtime: task=%s hook=%s", name, rt.TaskName, rt.Env["HOOK_TASK"])
						}
						return project.WriteFile(rt, name+".txt", []byte(rt.Env["HOOK_TASK"]), 0o644)
					},
				})
			}
			result := runValidation(t, p, t.TempDir(), mode, DefaultMaxOrders)
			if !result.Success {
				t.Fatalf("hook environment validation failed: %+v", result.Issues)
			}
			wantCalls := 1
			if mode == api.ValidationModeOrders {
				wantCalls = 2
			}
			if !reflect.DeepEqual(calls, map[string]int{"a": wantCalls, "b": wantCalls}) {
				t.Fatalf("hook calls = %v, want each task once per execution (%d)", calls, wantCalls)
			}
		})
	}
}

func TestValidationBeforeRunFailureStopsRunAndCapturesLog(t *testing.T) {
	for _, mode := range []api.ValidationMode{api.ValidationModeArtifacts, api.ValidationModeOrders} {
		t.Run(string(mode), func(t *testing.T) {
			runCalls := 0
			p := lifecycleProject(project.Task{
				Name: "check", Kind: project.KindOnce,
				BeforeRun: func(_ context.Context, rt *project.Runtime) error {
					rt.EmitLogLine("stderr", "hook setup diagnostic")
					return errors.New("hook setup failed")
				},
				Run: func(context.Context, *project.Runtime) error { runCalls++; return nil },
			})
			result := runValidation(t, p, t.TempDir(), mode, DefaultMaxOrders)
			if result.Success || runCalls != 0 {
				t.Fatalf("hook failure was bypassed: success=%v Run calls=%d", result.Success, runCalls)
			}
			errText, log := lifecycleFailure(t, result)
			if !strings.Contains(errText, "hook setup failed") || !strings.Contains(log, "hook setup diagnostic") {
				t.Fatalf("hook failure evidence: error=%q log=%q", errText, log)
			}
		})
	}
}

func TestValidationHookOnlyTaskProducesDependencyOutputs(t *testing.T) {
	for _, mode := range []api.ValidationMode{api.ValidationModeArtifacts, api.ValidationModeOrders} {
		t.Run(string(mode), func(t *testing.T) {
			worktree := t.TempDir()
			writeValidationFile(t, worktree, "source.txt", "from source")
			p := validationTestProject{
				name: "hook-only",
				tasks: []project.Task{
					{
						Name: "generate", Kind: project.KindOnce,
						Inputs: project.Inputs{Files: []string{"source.txt"}}, Outputs: project.Outputs{Files: []string{"generated.txt"}},
						BeforeRun: func(_ context.Context, rt *project.Runtime) error {
							data, err := os.ReadFile(rt.Abs("source.txt"))
							if err != nil {
								return err
							}
							return project.WriteFile(rt, "generated.txt", data, 0o644)
						},
					},
					{
						Name: "package", Kind: project.KindOnce, Deps: []string{"generate"},
						Outputs: project.Outputs{Files: []string{"package.txt"}},
						Run: func(_ context.Context, rt *project.Runtime) error {
							data, err := os.ReadFile(rt.Abs("generated.txt"))
							if err != nil {
								return err
							}
							if string(data) != "from source" {
								return fmt.Errorf("unexpected hook artifact %q", data)
							}
							return project.WriteFile(rt, "package.txt", data, 0o644)
						},
					},
				},
				targets: []project.Target{{Name: "build", RootTasks: []string{"package"}}},
			}
			result := runValidation(t, p, worktree, mode, DefaultMaxOrders)
			if !result.Success {
				t.Fatalf("hook-only producer failed validation: %+v", result.Issues)
			}
			if _, err := os.Stat(filepath.Join(worktree, "generated.txt")); !os.IsNotExist(err) {
				t.Fatalf("hook escaped the sandbox: %v", err)
			}
		})
	}
}

func TestArtifactValidationObservesUndeclaredHookWrites(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(fmt.Sprintf("fails=%v", fails), func(t *testing.T) {
			p := lifecycleProject(project.Task{
				Name: "generate", Kind: project.KindOnce, Outputs: project.Outputs{Files: []string{"output.txt"}},
				BeforeRun: func(_ context.Context, rt *project.Runtime) error {
					if err := project.WriteFile(rt, "hook.tmp", []byte("undeclared"), 0o644); err != nil {
						return err
					}
					if fails {
						return errors.New("hook failed after writing")
					}
					return nil
				},
				Run: func(_ context.Context, rt *project.Runtime) error {
					return project.WriteFile(rt, "output.txt", []byte("declared"), 0o644)
				},
			})
			result := runValidation(t, p, t.TempDir(), api.ValidationModeArtifacts, DefaultMaxOrders)
			if result.Success || !hasIssueKind(result.Issues, "undeclared_output") {
				t.Fatalf("undeclared hook write was not rejected: success=%v issues=%+v", result.Success, result.Issues)
			}
			if got := result.Artifacts.Tasks[0].UndeclaredWrites; !reflect.DeepEqual(got, []string{"hook.tmp"}) {
				t.Fatalf("undeclared writes = %v, want [hook.tmp]", got)
			}
		})
	}
}

func TestOrderValidationIncludesHookReadsAndWrites(t *testing.T) {
	p := validationTestProject{
		name: "hook-order",
		tasks: []project.Task{
			{
				Name: "a", Kind: project.KindOnce, Outputs: project.Outputs{Files: []string{"a.txt"}},
				BeforeRun: func(_ context.Context, rt *project.Runtime) error {
					return project.WriteFile(rt, "a.txt", []byte("ready"), 0o644)
				},
			},
			{
				Name: "b", Kind: project.KindOnce,
				BeforeRun: func(_ context.Context, rt *project.Runtime) error {
					_, err := os.ReadFile(rt.Abs("a.txt"))
					if err != nil {
						return fmt.Errorf("hook needs task a: %w", err)
					}
					return nil
				},
			},
		},
		targets: []project.Target{{Name: "build", RootTasks: []string{"a", "b"}}},
	}
	result := runValidation(t, p, t.TempDir(), api.ValidationModeOrders, DefaultMaxOrders)
	if result.Success || result.Orders == nil || len(result.Orders.Runs) != 2 {
		t.Fatalf("expected exhaustive missing-dependency failure: %+v", result)
	}
	first, second := result.Orders.Runs[0], result.Orders.Runs[1]
	if !first.Success || !reflect.DeepEqual(first.Tasks, []string{"a", "b"}) || second.Success || second.FailedTask != "b" || !strings.Contains(second.Error, "hook needs task a") {
		t.Fatalf("expected a->b success and b->a hook failure: %+v", result.Orders.Runs)
	}
}

func TestValidationCleansHandlesRegisteredByBeforeRun(t *testing.T) {
	for _, mode := range []api.ValidationMode{api.ValidationModeArtifacts, api.ValidationModeOrders} {
		for _, fails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fails=%v", mode, fails), func(t *testing.T) {
				handle := &lifecycleHandle{}
				runCalls := 0
				p := lifecycleProject(project.Task{
					Name: "check", Kind: project.KindOnce,
					BeforeRun: func(_ context.Context, rt *project.Runtime) error {
						rt.RegisterServiceHandle(handle)
						if fails {
							return errors.New("hook failed after registration")
						}
						return nil
					},
					Run: func(context.Context, *project.Runtime) error { runCalls++; return nil },
				})
				result := runValidation(t, p, t.TempDir(), mode, DefaultMaxOrders)
				if result.Success || handle.stops != 1 {
					t.Fatalf("hook resource not rejected and cleaned: success=%v stop calls=%d", result.Success, handle.stops)
				}
				errText, _ := lifecycleFailure(t, result)
				want := "supervised service"
				if fails {
					want = "hook failed after registration"
					if runCalls != 0 {
						t.Fatalf("Run called %d times after failed hook", runCalls)
					}
				}
				if !strings.Contains(errText, want) {
					t.Fatalf("error = %q, want %q", errText, want)
				}
			})
		}
	}
}

func TestValidationRejectsPromptsFromBeforeRun(t *testing.T) {
	for _, mode := range []api.ValidationMode{api.ValidationModeArtifacts, api.ValidationModeOrders} {
		t.Run(string(mode), func(t *testing.T) {
			runCalls := 0
			p := lifecycleProject(project.Task{
				Name: "check", Kind: project.KindOnce,
				BeforeRun: func(_ context.Context, rt *project.Runtime) error {
					_, err := rt.OnPrompt(rt.TaskName, process.PromptRequest{Prompt: "Proceed?"})
					return err
				},
				Run: func(context.Context, *project.Runtime) error { runCalls++; return nil },
			})
			result := runValidation(t, p, t.TempDir(), mode, DefaultMaxOrders)
			if result.Success || runCalls != 0 {
				t.Fatalf("hook prompt was bypassed: success=%v Run calls=%d", result.Success, runCalls)
			}
			errText, _ := lifecycleFailure(t, result)
			if !strings.Contains(errText, `task "check" requested an interactive prompt during validation`) {
				t.Fatalf("unexpected prompt error: %q", errText)
			}
		})
	}
}

func TestValidationRejectsServiceKindBeforeCallingHook(t *testing.T) {
	for _, kind := range []project.Kind{project.KindService, project.KindDebugService} {
		t.Run(string(kind), func(t *testing.T) {
			calls := 0
			p := lifecycleProject(project.Task{
				Name: "service", Kind: kind,
				BeforeRun: func(context.Context, *project.Runtime) error { calls++; return nil },
			})
			result := runValidation(t, p, t.TempDir(), api.ValidationModeAll, DefaultMaxOrders)
			if result.Success || calls != 0 || !hasIssueKind(result.Issues, "unsupported_task_kind") {
				t.Fatalf("service preflight bypassed: success=%v hook calls=%d issues=%+v", result.Success, calls, result.Issues)
			}
		})
	}
}

func TestValidationLifecyclePreservesCleanupFailures(t *testing.T) {
	for _, phase := range []string{"hook", "run"} {
		for _, cleanup := range []string{"stop_error", "still_alive"} {
			t.Run(phase+"/"+cleanup, func(t *testing.T) {
				taskErr := errors.New("task lifecycle failed")
				stopErr := errors.New("stop failed")
				handle := &lifecycleHandle{remainsAlive: cleanup == "still_alive"}
				otherHandle := &lifecycleHandle{}
				if cleanup == "stop_error" {
					handle.stopErr = stopErr
				}
				fail := func(_ context.Context, rt *project.Runtime) error {
					rt.RegisterServiceHandle(handle)
					rt.RegisterServiceHandle(otherHandle)
					return taskErr
				}
				task := project.Task{Name: "check", Kind: project.KindOnce}
				if phase == "hook" {
					task.BeforeRun = fail
				} else {
					task.Run = fail
				}
				runtime, err := (runtimeTemplate{}).runtime(t.TempDir(), "artifacts")
				if err != nil {
					t.Fatal(err)
				}
				_, err = runtime.runTask(context.Background(), task, filepath.Join(t.TempDir(), "check.log"), nil)
				if handle.stops != 1 || otherHandle.stops != 1 || !errors.Is(err, taskErr) {
					t.Fatalf("task failure or cleanup lost: error=%v stop calls=(%d, %d)", err, handle.stops, otherHandle.stops)
				}
				if cleanup == "stop_error" && !errors.Is(err, stopErr) {
					t.Fatalf("cleanup failure lost: %v", err)
				}
				if cleanup == "still_alive" && !strings.Contains(err.Error(), "alive") {
					t.Fatalf("live resource after Stop was not reported: %v", err)
				}
			})
		}
	}
}

func TestValidationLifecycleCancellationCleansHookHandle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle := &lifecycleHandle{}
	runCalls := 0
	task := project.Task{
		Name: "check", Kind: project.KindOnce,
		BeforeRun: func(ctx context.Context, rt *project.Runtime) error {
			rt.RegisterServiceHandle(handle)
			cancel()
			return ctx.Err()
		},
		Run: func(context.Context, *project.Runtime) error { runCalls++; return nil },
	}
	runtime, err := (runtimeTemplate{}).runtime(t.TempDir(), "artifacts")
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.runTask(ctx, task, filepath.Join(t.TempDir(), "check.log"), nil)
	if !errors.Is(err, context.Canceled) || runCalls != 0 || handle.stops != 1 {
		t.Fatalf("hook cancellation lifecycle: error=%v Run calls=%d stop calls=%d", err, runCalls, handle.stops)
	}
}

func lifecycleProject(task project.Task) validationTestProject {
	return validationTestProject{name: "hook-lifecycle", tasks: []project.Task{task}, targets: []project.Target{{Name: "build", RootTasks: []string{task.Name}}}}
}

func lifecycleFailure(t *testing.T, result *api.ValidationResult) (string, string) {
	t.Helper()
	if result.Artifacts != nil && len(result.Artifacts.Tasks) == 1 {
		return result.Artifacts.Tasks[0].Error, result.Artifacts.Tasks[0].Log
	}
	if result.Orders != nil && len(result.Orders.Runs) == 1 {
		return result.Orders.Runs[0].Error, result.Orders.Runs[0].Log
	}
	t.Fatalf("expected one failed task or order: %+v", result)
	return "", ""
}

type lifecycleHandle struct {
	stops        int
	stopErr      error
	remainsAlive bool
}

func (*lifecycleHandle) PID() int      { return 0 }
func (h *lifecycleHandle) Alive() bool { return h.stops == 0 || h.remainsAlive }
func (*lifecycleHandle) Wait() error   { return nil }
func (h *lifecycleHandle) Stop() error { h.stops++; return h.stopErr }
