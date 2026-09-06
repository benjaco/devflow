// Package executionstate admits worktree execution only after both the lease
// and resources retained after an interrupted execution have been checked.
package executionstate

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/benjaco/devflow/internal/execution"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

// Acquire reserves the worktree before inspecting resources left by interrupted
// execution. Refusal leaves the previous execution snapshots intact and
// removes the rejected contender's new ownership record.
func Acquire(worktree string, owner execution.Owner, opts ...execution.Option) (*execution.Lease, error) {
	lease, err := execution.Acquire(worktree, owner, opts...)
	if err != nil {
		return nil, err
	}
	if err := CheckIdle(lease.Owner().Worktree); err != nil {
		return nil, errors.Join(err, lease.Release())
	}
	return lease, nil
}

// CheckIdle is read-only. Call it while holding the execution lease when its
// result will authorize mutation. An idle daemon with separate control metadata
// does not own execution resources; its active engine must hold the lease.
func CheckIdle(worktree string) error {
	id, root, err := instance.IDForWorktree(worktree)
	if err != nil {
		return err
	}
	owner := execution.Owner{Worktree: root, Kind: "persisted_resource"}
	conflict := func(pid int, cause error) error {
		owner.PID = pid
		return &execution.ConflictError{Owner: &owner, RecoveryRequired: true, Cause: cause}
	}
	inst, err := instance.Load(root, id)
	if err != nil && !os.IsNotExist(err) {
		return conflict(0, fmt.Errorf("inspect persisted instance: %w", err))
	}
	if inst != nil {
		owner.Target = inst.LastRun.Target
		owner.Mode = string(inst.LastRun.Mode)
	}
	state, err := instance.LoadStatus(root, id)
	if err != nil && !os.IsNotExist(err) {
		return conflict(0, fmt.Errorf("inspect persisted task status: %w", err))
	}
	if state != nil {
		if state.Target != "" {
			owner.Target = state.Target
		}
		if state.Mode != "" {
			owner.Mode = string(state.Mode)
		}
	}
	if inst != nil {
		names := make([]string, 0, len(inst.Processes))
		for name := range inst.Processes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if pid := inst.Processes[name].PID; instance.ProcessAlive(pid) {
				return conflict(pid, fmt.Errorf("recorded process %q is still alive", name))
			}
		}
	}
	if state != nil {
		names := make([]string, 0, len(state.Nodes))
		for name := range state.Nodes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			node := state.Nodes[name]
			if instance.ProcessAlive(node.PID) {
				return conflict(node.PID, fmt.Errorf("task %q still has a live recorded process", name))
			}
			if node.PID > 0 {
				continue
			}
			switch node.State {
			case api.StateRunning, api.StateStarting, api.StateReady, api.StateRestarting, api.StateDegraded:
				return conflict(0, fmt.Errorf("task %q has unresolved %s state without a process identity", name, node.State))
			}
		}
	}
	return nil
}
