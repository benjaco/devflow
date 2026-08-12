package project

import (
	"sync/atomic"
	"testing"

	"github.com/benjaco/devflow/pkg/process"
)

type inertServiceHandle struct{}

func (inertServiceHandle) PID() int    { return 0 }
func (inertServiceHandle) Alive() bool { return true }
func (inertServiceHandle) Wait() error { return nil }
func (inertServiceHandle) Stop() error { return nil }

func TestRegisterServiceHandleUsesGenericLifecycleHook(t *testing.T) {
	var calls atomic.Int32
	runtime := &Runtime{
		TaskName: "database",
		OnServiceHandle: func(task string, handle ServiceHandle) {
			if task != "database" || handle == nil {
				t.Fatalf("unexpected service registration: task=%q handle=%v", task, handle)
			}
			calls.Add(1)
		},
	}
	runtime.RegisterServiceHandle(inertServiceHandle{})
	if got := calls.Load(); got != 1 {
		t.Fatalf("generic service registrations = %d, want 1", got)
	}
}

func TestRegisterServiceHandleRetainsLegacyProcessHook(t *testing.T) {
	handle := &process.Handle{}
	var registered *process.Handle
	runtime := &Runtime{
		TaskName: "app",
		OnService: func(task string, got *process.Handle) {
			if task != "app" {
				t.Fatalf("legacy service task = %q", task)
			}
			registered = got
		},
	}
	runtime.RegisterServiceHandle(handle)
	if registered != handle {
		t.Fatal("legacy process-only service hook was not preserved")
	}
}
