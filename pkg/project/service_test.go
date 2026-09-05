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

func TestRegisterServiceHandleUsesOneLifecycleHook(t *testing.T) {
	for name, handle := range map[string]ServiceHandle{
		"process": &process.Handle{},
		"managed": inertServiceHandle{},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			runtime := &Runtime{
				TaskName: "service",
				OnServiceHandle: func(task string, got ServiceHandle) {
					if task != "service" || got != handle {
						t.Fatalf("unexpected service registration: task=%q handle=%v", task, got)
					}
					calls.Add(1)
				},
			}
			runtime.RegisterServiceHandle(handle)
			if got := calls.Load(); got != 1 {
				t.Fatalf("service registrations = %d, want 1", got)
			}
		})
	}
}
