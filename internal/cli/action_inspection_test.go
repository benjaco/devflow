package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/execution"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/daemon"
	"github.com/benjaco/devflow/pkg/instance"
)

func TestActionListDoesNotStartDaemon(t *testing.T) {
	isolateJSONContractState(t)
	restore := daemon.SetStartDaemonFuncForTest(func(string, string, string) error {
		t.Error("action inspection attempted to start a daemon")
		return errors.New("inspection must not start a daemon")
	})
	t.Cleanup(restore)
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			worktree := t.TempDir()
			args := []string{"action", "list", "--project", "cli-action-project", "--worktree", worktree}
			if format == "json" {
				args = append(args, "--json")
			}
			var stdout, stderr bytes.Buffer
			app := &App{Stdout: &stdout, Stderr: &stderr}
			if err := app.Run(args); err != nil {
				t.Fatalf("inspection failed: %v; stdout=%s stderr=%s", err, &stdout, &stderr)
			}
			if !strings.Contains(stdout.String(), "db.migration.create") {
				t.Fatalf("missing declared action: %s", &stdout)
			}
			if _, err := os.Stat(filepath.Join(worktree, ".devflow")); !os.IsNotExist(err) {
				t.Fatalf("inspection created runtime state: %v", err)
			}
		})
	}
}

func TestBootstrapActionInspectionPreservesWatcherAfterAdapterRebuild(t *testing.T) {
	isolateJSONContractState(t)
	worktree := t.TempDir()
	writeLocalProjectFile(t, worktree, actionInspectionProjectSource)
	processes := map[int]bool{}
	t.Cleanup(func() {
		stopJSONContractDaemon(t, worktree)
		// Socket closure can precede process exit and release of the mapped
		// daemon executable on Windows. Wait before TempDir removes the files.
		deadline := time.Now().Add(5 * time.Second)
		for pid := range processes {
			for instance.ProcessAlive(pid) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if instance.ProcessAlive(pid) {
				t.Errorf("fixture process %d survived cleanup", pid)
			}
		}
	})
	call := func(t *testing.T, args ...string) string {
		t.Helper()
		stdout, stderr, err := runJSONContractCommand(t, worktree, args...)
		if err != nil {
			t.Fatalf("%q failed: %v\nstdout=%s\nstderr=%s", args, err, stdout, stderr)
		}
		return stdout
	}
	status := func(t *testing.T) api.StatusResult {
		t.Helper()
		var result api.StatusResult
		if err := json.Unmarshal([]byte(call(t, "status", "--details", "full", "--json")), &result); err != nil {
			t.Fatal(err)
		}
		if result.Daemon != nil {
			processes[result.Daemon.PID] = true
		}
		for _, node := range result.Nodes {
			if node.PID > 0 {
				processes[node.PID] = true
			}
		}
		return result
	}
	call(t, "watch", "up", "--detach", "--json")
	call(t, "flush", "up", "--timeout", "30s", "--json")
	before := status(t)
	if before.Daemon == nil || len(before.Nodes) != 1 {
		t.Fatalf("missing watcher/service: %+v", before)
	}
	service := before.Nodes[0]
	if service.State != api.StateRunning || !service.Ready || service.PID <= 0 || service.RunID == "" || service.AttemptID == "" {
		t.Fatalf("fixture service not ready: %+v", service)
	}
	owner, err := execution.ReadOwner(worktree)
	if err != nil || owner == nil || owner.PID != before.Daemon.PID {
		t.Fatalf("missing daemon execution owner: %+v, %v", owner, err)
	}
	// The unchanged control alone misses this bug: replacement happens only
	// after bootstrap loads a different adapter executable.
	for _, change := range []struct{ name, source, label string }{
		{"unchanged", actionInspectionProjectSource, "Original action"},
		{"comment", actionInspectionProjectSource + "\n// Rebuild the adapter without changing its declarations.\n", "Original action"},
		{"metadata", strings.ReplaceAll(actionInspectionProjectSource, "Original action", "Updated action"), "Updated action"},
	} {
		t.Run(change.name, func(t *testing.T) {
			writeLocalProjectFile(t, worktree, change.source)
			var actions actionListResult
			if err := json.Unmarshal([]byte(call(t, "action", "list", "--json")), &actions); err != nil {
				t.Fatal(err)
			}
			if actions.Project != "action-inspection" || len(actions.Actions) != 1 || actions.Actions[0].Label != change.label {
				t.Fatalf("inspection did not load current adapter metadata: %+v", actions)
			}
			after := status(t)
			if !reflect.DeepEqual(after.Daemon, before.Daemon) || after.RunID != before.RunID || !reflect.DeepEqual(after.Nodes, before.Nodes) {
				t.Errorf("inspection changed watcher/service: before=%+v daemon=%+v; after=%+v daemon=%+v", before, before.Daemon, after, after.Daemon)
			}
			if !instance.ProcessAlive(before.Daemon.PID) || !instance.ProcessAlive(service.PID) {
				t.Error("inspection stopped the original daemon or service")
			}
			afterOwner, err := execution.ReadOwner(worktree)
			if err != nil || !reflect.DeepEqual(afterOwner, owner) {
				t.Errorf("inspection changed execution ownership: before=%+v after=%+v err=%v", owner, afterOwner, err)
			}
			lease, err := execution.Acquire(worktree, execution.Owner{Kind: "inspection-test"})
			if lease != nil {
				_ = lease.Release()
			}
			var conflict *execution.ConflictError
			if !errors.As(err, &conflict) || conflict.RecoveryRequired || conflict.Owner == nil || conflict.Owner.Token != owner.Token {
				t.Errorf("watcher no longer holds its execution lease: %v", err)
			}
		})
		if t.Failed() {
			break
		}
	}
}

const actionInspectionProjectSource = `package main

import (
	"context"
	"os"
	"time"

	"github.com/benjaco/devflow/pkg/project"
)

func init() {
	if len(os.Args) == 2 && os.Args[1] == "--inspection-sleep-helper" {
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}
	project.Register(project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("action-inspection")
		b.DefaultTarget("up")
		// The daemon copy remains executable while devflow-local is rebuilt,
		// including on Windows where running executables cannot be replaced.
		executable, err := os.Executable()
		if err != nil { return err }
		service := b.Service("harmless").Command(executable, "--inspection-sleep-helper").NoCache().RestartNever()
		b.Target("up", service)
		b.Action("inspectable").Label("Original action")
		return nil
	}))
}
`
