package executionstate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/benjaco/devflow/internal/execution"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

func TestAcquireRejectsUnleasedPersistedResources(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*api.Instance, map[string]api.NodeStatus)
	}{
		{"process registry", func(inst *api.Instance, _ map[string]api.NodeStatus) {
			inst.Processes["service"] = api.ProcessRef{PID: os.Getpid()}
		}},
		{"status process", func(_ *api.Instance, nodes map[string]api.NodeStatus) {
			nodes["service"] = api.NodeStatus{Name: "service", Kind: "service", State: api.StateRunning, PID: os.Getpid()}
		}},
		{"pidless service", func(_ *api.Instance, nodes map[string]api.NodeStatus) {
			nodes["service"] = api.NodeStatus{Name: "service", Kind: "service", State: api.StateRunning}
		}},
		{"degraded unknown kind", func(_ *api.Instance, nodes map[string]api.NodeStatus) {
			nodes["resource"] = api.NodeStatus{Name: "resource", State: api.StateDegraded}
		}},
		{"interrupted starting task", func(_ *api.Instance, nodes map[string]api.NodeStatus) {
			nodes["check"] = api.NodeStatus{Name: "check", Kind: "once", State: api.StateStarting}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := idleInstance(t)
			nodes := map[string]api.NodeStatus{}
			tc.edit(inst, nodes)
			if err := instance.Save(inst); err != nil {
				t.Fatal(err)
			}
			if err := instance.SaveStatus(inst.Worktree, inst.ID, "development", api.ModeWatch, nodes); err != nil {
				t.Fatal(err)
			}
			stateDir := filepath.Join(inst.Worktree, ".devflow", "state", "instances", inst.ID)
			before := map[string][]byte{}
			for _, name := range []string{"instance.json", "runtime.env", "status.json"} {
				data, err := os.ReadFile(filepath.Join(stateDir, name))
				if err != nil {
					t.Fatal(err)
				}
				before[name] = data
			}
			lease, err := Acquire(inst.Worktree, execution.Owner{Target: "verify", Mode: "ci"})
			if lease != nil {
				_ = lease.Release()
				t.Error("admitted execution over unreconciled resource")
			}
			var conflict *execution.ConflictError
			if !errors.As(err, &conflict) || !conflict.RecoveryRequired || conflict.Owner == nil || conflict.Owner.Target != "development" {
				t.Errorf("expected diagnostic recovery conflict, got %v", err)
			}
			if owner, err := execution.ReadOwner(inst.Worktree); err != nil || owner != nil {
				t.Errorf("rejected contender left ownership marker: owner=%+v err=%v", owner, err)
			}
			for name, expected := range before {
				actual, err := os.ReadFile(filepath.Join(stateDir, name))
				if err != nil || !bytes.Equal(expected, actual) {
					t.Errorf("admission changed %s: %v", name, err)
				}
			}
		})
	}
}

func TestAcquireAllowsIdleDaemonAndCompletedExecution(t *testing.T) {
	inst := idleInstance(t)
	inst.DB.ContainerName = "persisted-runtime-config"
	inst.Processes["dead"] = api.ProcessRef{PID: 1 << 30}
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}
	if err := instance.RecordDaemon(inst, os.Getpid(), "daemon.log"); err != nil {
		t.Fatal(err)
	}
	if err := instance.SaveStatus(inst.Worktree, inst.ID, "previous", api.ModeCI, map[string]api.NodeStatus{
		"completed": {Name: "completed", Kind: "once", State: api.StateDone},
		"cached":    {Name: "cached", Kind: "once", State: api.StateCached},
		"stopped":   {Name: "stopped", Kind: "service", State: api.StateStopped},
		"dead":      {Name: "dead", Kind: "service", State: api.StateRunning, PID: 1 << 30},
		"exited":    {Name: "exited", Kind: "service", State: api.StateFailed, Generation: 1},
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := Acquire(inst.Worktree, execution.Owner{Target: "verify", Mode: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckIdleIsReadOnlyBeforeFirstExecution(t *testing.T) {
	root := t.TempDir()
	if err := CheckIdle(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".devflow")); !os.IsNotExist(err) {
		t.Fatalf("read-only admission inspection created state: %v", err)
	}
}

func TestAcquirePreservesCorruptExecutionState(t *testing.T) {
	inst := idleInstance(t)
	path := filepath.Join(inst.Worktree, ".devflow", "state", "instances", inst.ID, "status.json")
	if err := os.WriteFile(path, []byte(`{"nodes":`), 0o600); err != nil {
		t.Fatal(err)
	}
	lease, err := Acquire(inst.Worktree, execution.Owner{Target: "verify", Mode: "ci"})
	if lease != nil {
		_ = lease.Release()
		t.Error("admitted unknown corrupt execution state")
	}
	var conflict *execution.ConflictError
	if !errors.As(err, &conflict) || !conflict.RecoveryRequired {
		t.Fatalf("expected recovery conflict, got %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"nodes":` {
		t.Fatalf("corrupt state was changed: %q, %v", data, err)
	}
	if owner, err := execution.ReadOwner(inst.Worktree); err != nil || owner != nil {
		t.Fatalf("rejected contender retained marker: %+v, %v", owner, err)
	}
}

func idleInstance(t *testing.T) *api.Instance {
	t.Helper()
	cacheRoot := t.TempDir()
	t.Setenv("HOME", cacheRoot)
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("LOCALAPPDATA", cacheRoot)
	inst, err := instance.Resolve(t.TempDir(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	inst.Env["PRESERVE"] = "owner"
	return inst
}
