package cli

import (
	"encoding/json"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

func TestCompactServiceCountsFromSnapshot(t *testing.T) {
	ready := api.NodeStatus{Name: "ready", Kind: string(project.KindService), State: api.StateRunning, Ready: true, PID: 1234}
	pidless := api.NodeStatus{Name: "container", Kind: string(project.KindService), State: api.StateRunning, Ready: true}
	debug := api.NodeStatus{Name: "debug", Kind: string(project.KindDebugService), State: api.StateRunning, Ready: true, PID: 1235}
	for _, tc := range []struct {
		name     string
		nodes    []api.NodeStatus
		services int
		unready  int
	}{
		{name: "ready", nodes: []api.NodeStatus{ready}, services: 1},
		{name: "pidless", nodes: []api.NodeStatus{pidless}, services: 1},
		{name: "debug", nodes: []api.NodeStatus{debug}, services: 1},
		{name: "running-unready", nodes: []api.NodeStatus{{Name: "waiting", Kind: string(project.KindService), State: api.StateRunning}}, services: 1, unready: 1},
		{name: "pending", nodes: []api.NodeStatus{{Name: "pending", Kind: string(project.KindService), State: api.StatePending}}, services: 1, unready: 1},
		{name: "stopped", nodes: []api.NodeStatus{{Name: "stopped", Kind: string(project.KindService), State: api.StateStopped}}, services: 1, unready: 1},
		{name: "degraded-debug", nodes: []api.NodeStatus{{Name: "degraded", Kind: string(project.KindDebugService), State: api.StateDegraded}}, services: 1, unready: 1},
		{name: "finite", nodes: []api.NodeStatus{{Name: "build", Kind: string(project.KindOnce), State: api.StateDone}}},
		{name: "empty"},
		{name: "mixed", nodes: []api.NodeStatus{ready, pidless, debug,
			{Name: "stopped", Kind: string(project.KindService), State: api.StateStopped},
			{Name: "starting", Kind: string(project.KindDebugService), State: api.StateStarting},
			{Name: "build", Kind: string(project.KindOnce), State: api.StateDone},
		}, services: 5, unready: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := api.RunResult{RunID: "run", InstanceID: "instance", Nodes: tc.nodes}
			for _, result := range []struct {
				name  string
				value any
			}{
				{name: "status", value: api.StatusResult{RunID: "run", InstanceID: "instance", Nodes: tc.nodes}},
				{name: "run", value: run},
				{name: "run-pointer", value: &run},
			} {
				t.Run(result.name, func(t *testing.T) {
					original, err := json.Marshal(result.value)
					if err != nil {
						t.Fatal(err)
					}
					for _, details := range []string{"summary", "issues"} {
						t.Run(details, func(t *testing.T) {
							view := (&App{details: details}).resultView(result.value).(*api.ExecutionView)
							if view.Counts.Services != tc.services || view.Counts.UnreadyServices != tc.unready {
								t.Errorf("service counts = %d/%d, want %d/%d", view.Counts.Services, view.Counts.UnreadyServices, tc.services, tc.unready)
							}
							if view.Counts.Nodes != len(tc.nodes) {
								t.Errorf("nodes = %d, want %d", view.Counts.Nodes, len(tc.nodes))
							}
						})
					}
					full := (&App{details: "full"}).resultView(result.value)
					after, err := json.Marshal(full)
					if err != nil {
						t.Fatal(err)
					}
					if string(after) != string(original) {
						t.Fatalf("presentation changed full evidence: %s", after)
					}
				})
			}
		})
	}
}

func TestCompactFlushServiceCountsUseLiveProbes(t *testing.T) {
	result := api.FlushResult{
		Nodes: []api.NodeStatus{
			{Name: "healthy", Kind: string(project.KindService), State: api.StateRunning, Ready: true},
			{Name: "exited", Kind: string(project.KindDebugService), State: api.StateRunning, Ready: true},
			{Name: "probe-failed", Kind: string(project.KindService), State: api.StateRunning, Ready: true},
			{Name: "not-probed", Kind: string(project.KindService), State: api.StatePending},
		},
		Services: []api.FlushService{
			{Task: "healthy", Alive: true, Ready: true},
			{Task: "exited", Alive: false, Ready: true},
			{Task: "probe-failed", Alive: true, Ready: false},
		},
	}
	for _, details := range []string{"summary", "issues"} {
		t.Run(details, func(t *testing.T) {
			view := (&App{details: details}).resultView(result).(*api.ExecutionView)
			if view.Counts.Services != 3 || view.Counts.UnreadyServices != 2 || view.Counts.Nodes != 4 {
				t.Fatalf("counts must use service probes without counting nodes again: %+v", view.Counts)
			}
			resultWithoutProbes := result
			resultWithoutProbes.Services = nil
			view = (&App{details: details}).resultView(resultWithoutProbes).(*api.ExecutionView)
			if view.Counts.Services != 0 || view.Counts.UnreadyServices != 0 {
				t.Fatalf("missing probes must not fall back to node readiness: %+v", view.Counts)
			}
		})
	}
}
