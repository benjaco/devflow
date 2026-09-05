package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

type taskLifecycleProject struct{ task project.Task }

func (taskLifecycleProject) Name() string { return "task-lifecycle" }
func (taskLifecycleProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Env: map[string]string{"VALUE": "base"}}, nil
}
func (p taskLifecycleProject) Tasks() []project.Task { return []project.Task{p.task} }
func (p taskLifecycleProject) Targets() []project.Target {
	return []project.Target{{Name: "verify", RootTasks: []string{p.task.Name}}}
}

func TestTaskLifecycleCarriesHookRuntimeThroughReadiness(t *testing.T) {
	root := t.TempDir()
	var phases []string
	var hookRuntime *project.Runtime
	handle := newGenericServiceHandle()
	checkRuntime := func(rt *project.Runtime, phase, value string) error {
		if rt != hookRuntime || rt.Env["VALUE"] != value {
			return errors.New("task runtime was lost between lifecycle callbacks")
		}
		phases = append(phases, phase)
		return nil
	}
	p := taskLifecycleProject{task: project.Task{
		Name: "serve", Kind: project.KindService,
		BeforeRun: func(_ context.Context, rt *project.Runtime) error {
			hookRuntime = rt
			rt.Env["VALUE"] = "hook"
			phases = append(phases, "before")
			return nil
		},
		Run: func(_ context.Context, rt *project.Runtime) error {
			if err := checkRuntime(rt, "run", "hook"); err != nil {
				return err
			}
			rt.Env["VALUE"] = "run"
			rt.RegisterServiceHandle(handle)
			return nil
		},
		Ready: func(_ context.Context, rt *project.Runtime) error {
			return checkRuntime(rt, "ready", "run")
		},
		AfterReady: func(_ context.Context, rt *project.Runtime) error {
			return checkRuntime(rt, "after", "run")
		},
	}}
	eng, err := New(p, root)
	if err != nil {
		t.Fatal(err)
	}
	out, err := eng.Run(context.Background(), Request{Worktree: root, Target: "verify", Mode: api.ModeCI})
	if err != nil || !out.Result.Success {
		t.Fatalf("run failed: outcome=%+v error=%v", out, err)
	}
	if want := []string{"before", "run", "ready", "after"}; !reflect.DeepEqual(phases, want) {
		t.Errorf("lifecycle phases = %v, want %v", phases, want)
	}
	if out.Instance.Env["VALUE"] != "base" {
		t.Errorf("task mutated instance env: %v", out.Instance.Env)
	}
	if handle.Alive() {
		t.Error("CI left the hook-configured service running")
	}
}

func TestTaskLifecycleHookFailureStopsRegisteredHandle(t *testing.T) {
	root := t.TempDir()
	handle := newGenericServiceHandle()
	hookErr := errors.New("hook failed")
	ran := false
	p := taskLifecycleProject{task: project.Task{
		Name: "prepare", Kind: project.KindOnce,
		BeforeRun: func(_ context.Context, rt *project.Runtime) error {
			rt.RegisterServiceHandle(handle)
			return hookErr
		},
		Run: func(context.Context, *project.Runtime) error { ran = true; return nil },
	}}
	eng, err := New(p, root)
	if err != nil {
		t.Fatal(err)
	}
	out, err := eng.Run(context.Background(), Request{Worktree: root, Target: "verify", Mode: api.ModeCI})
	if !errors.Is(err, hookErr) || out == nil || out.Result.Success {
		t.Fatalf("hook failure lost: outcome=%+v error=%v", out, err)
	}
	if ran || handle.Alive() {
		t.Errorf("failed hook lifecycle: run called=%v service alive=%v", ran, handle.Alive())
	}
}
