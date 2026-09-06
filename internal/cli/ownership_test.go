package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/benjaco/devflow/internal/execution"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/cache"
	"github.com/benjaco/devflow/pkg/daemon"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

type ownershipCLIProject struct {
	configured  atomic.Int32
	executed    atomic.Int32
	fingerprint atomic.Int32
}

func (*ownershipCLIProject) Name() string { return "cli-ownership-project" }

func (p *ownershipCLIProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	p.configured.Add(1)
	return project.InstanceConfig{Label: "contender", Env: map[string]string{"OWNERSHIP_VALUE": "contender"}}, nil
}

func (p *ownershipCLIProject) Tasks() []project.Task {
	return []project.Task{{
		Name: "check", Kind: project.KindOnce, Cache: true,
		Inputs: project.Inputs{Files: []string{"input.txt"}, Custom: []project.FingerprintFunc{
			func(context.Context, *project.Runtime) (string, error) {
				p.fingerprint.Add(1)
				return "fingerprint", nil
			},
		}},
		Outputs: project.Outputs{Files: []string{"artifact.txt"}},
		Run: func(_ context.Context, rt *project.Runtime) error {
			p.executed.Add(1)
			return os.WriteFile(rt.Abs("artifact.txt"), []byte("contender"), 0o600)
		},
	}}
}

func (*ownershipCLIProject) Targets() []project.Target {
	return []project.Target{{Name: "verify", RootTasks: []string{"check"}}}
}

func TestCIOwnershipConflictIsStructuredAndPreservesActiveEvidence(t *testing.T) {
	f := newCLIOwnershipFixture(t)
	var daemonStarts atomic.Int32
	restore := daemon.SetStartDaemonFuncForTest(func(string, string, string) error {
		daemonStarts.Add(1)
		return errors.New("direct CI must not start a daemon")
	})
	t.Cleanup(restore)
	f.runRejected(t, []string{"run", "verify", "--ci", "--json"})
	if daemonStarts.Load() != 0 {
		t.Errorf("direct CI started %d daemons", daemonStarts.Load())
	}
}

func TestCacheKeyOwnershipConflictDoesNotPrepareExecution(t *testing.T) {
	f := newCLIOwnershipFixture(t)
	manifestPath := filepath.Join(f.inst.Worktree, "manifest.json")
	f.runRejected(t, []string{"cache", "key", "--target", "verify", "--manifest-out", manifestPath, "--json"})
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Errorf("rejected cache-key command wrote a manifest: %v", err)
	}
}

func TestCacheInvalidateOwnershipConflictPreservesCacheAndStamps(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprintf("all=%t", all), func(t *testing.T) {
			f := newCLIOwnershipFixture(t)
			store := cache.NewNamespaced(instance.CacheRoot(), project.CacheNamespace(f.project))
			if _, err := store.Snapshot(f.inst.Worktree, f.project.Tasks()[0], "existing-key"); err != nil {
				t.Fatal(err)
			}
			stampPath := instance.TaskStampPath(f.inst.Worktree, f.inst.ID, "check")
			if err := instance.WriteTaskStamp(f.inst.Worktree, f.inst.ID, "check", "existing-key"); err != nil {
				t.Fatal(err)
			}
			stamp, err := os.ReadFile(stampPath)
			if err != nil {
				t.Fatal(err)
			}
			f.before[stampPath] = stamp
			args := []string{"cache", "invalidate", "--json"}
			if !all {
				args = append(args, "--task", "check")
			}
			f.runRejected(t, args)
			if _, found, err := store.Load("check", "existing-key"); err != nil || !found {
				t.Errorf("rejected invalidation removed cache entry: found=%t err=%v", found, err)
			}
		})
	}
}

func TestDaemonAdmissionOwnershipErrorsAreStructured(t *testing.T) {
	for _, command := range [][]string{
		{"run", "verify", "--json"},
		{"run", "verify", "--detach", "--json"},
		{"watch", "verify", "--detach", "--json"},
		{"flush", "verify", "--json"},
		{"restart", "check", "--json"},
	} {
		t.Run(command[0]+fmt.Sprint(command[2:]), func(t *testing.T) {
			f := newCLIOwnershipFixture(t)
			restore := daemon.SetStartDaemonFuncForTest(func(worktree, _, _ string) error {
				lease, err := execution.Acquire(worktree, execution.Owner{Target: "verify", Kind: "daemon"})
				if err == nil {
					_ = lease.Release()
					return errors.New("daemon startup unexpectedly obtained execution ownership")
				}
				return err
			})
			t.Cleanup(restore)
			f.runRejected(t, command)
		})
	}
}

func TestFlushOwnershipConflictRetainsDaemonIssues(t *testing.T) {
	issue := api.FlushIssue{Kind: "watch_start_error", Message: "watch could not acquire ownership"}
	owner := &execution.Owner{Worktree: "/worktree", PID: 1234, Target: "development", Mode: "watch"}
	result := preserveFlushCallError(api.FlushResult{RequestID: "flush-request", Issues: []api.FlushIssue{issue}},
		fmt.Errorf("daemon: %w", &execution.ConflictError{Owner: owner}), "/worktree", "instance", "project", "verify", 0)
	if result.Error == nil || result.Error.Code != "resource_conflict" || result.ResourceConflict == nil || result.ResourceConflict.Target != owner.Target || result.ResourceConflict.PID != owner.PID {
		t.Fatalf("flush discarded structured ownership error: %+v", result)
	}
	if result.RequestID != "flush-request" || len(result.Issues) != 1 || result.Issues[0] != issue {
		t.Fatalf("flush replaced existing daemon evidence: %+v", result)
	}
}

type cliOwnershipFixture struct {
	project *ownershipCLIProject
	inst    *api.Instance
	before  map[string][]byte
}

func newCLIOwnershipFixture(t *testing.T) *cliOwnershipFixture {
	t.Helper()
	cacheRoot := t.TempDir()
	t.Setenv("HOME", cacheRoot)
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("LOCALAPPDATA", cacheRoot)
	p := &ownershipCLIProject{}
	project.Register(p)
	inst, err := instance.Resolve(t.TempDir(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	inst.Env["OWNERSHIP_VALUE"] = "owner"
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}
	logPath := instance.LogPath(inst.Worktree, inst.ID, "check")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{logPath: "owner log\n", filepath.Join(inst.Worktree, "artifact.txt"): "owner artifact", filepath.Join(inst.Worktree, "input.txt"): "owner input"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := instance.SaveStatus(inst.Worktree, inst.ID, "development", api.ModeWatch, map[string]api.NodeStatus{
		"check": {Name: "check", State: api.StateRunning, LogPath: logPath},
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := execution.Acquire(inst.Worktree, execution.Owner{Target: "development", Mode: "watch", Kind: "daemon"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Release(); err != nil {
			t.Error(err)
		}
	})
	stateDir := filepath.Join(inst.Worktree, ".devflow", "state", "instances", inst.ID)
	before := map[string][]byte{}
	for _, path := range []string{filepath.Join(stateDir, "instance.json"), filepath.Join(stateDir, "runtime.env"), filepath.Join(stateDir, "status.json"), logPath, filepath.Join(inst.Worktree, "artifact.txt"), filepath.Join(inst.Worktree, ".devflow", "execution-owner.json")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	return &cliOwnershipFixture{project: p, inst: inst, before: before}
}

func (f *cliOwnershipFixture) runRejected(t *testing.T, command []string) {
	t.Helper()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: stderr}
	args := append(append([]string(nil), command...), "--project", f.project.Name(), "--worktree", f.inst.Worktree)
	err := app.Run(args)
	if err == nil {
		t.Error("command succeeded while another execution owned the worktree")
	}
	var result struct {
		Error            *api.CommandError     `json:"error"`
		ResourceConflict *api.ResourceConflict `json:"resourceConflict"`
	}
	decoder := json.NewDecoder(stdout)
	if decodeErr := decoder.Decode(&result); decodeErr != nil {
		t.Errorf("expected structured ownership conflict: decode=%v command=%v stderr=%s", decodeErr, err, stderr)
	} else {
		if result.Error == nil || result.Error.Code != "resource_conflict" || result.Error.Message == "" {
			t.Errorf("missing structured ownership error: %+v", result)
		}
		if result.ResourceConflict == nil || result.ResourceConflict.Target != "development" || result.ResourceConflict.PID != os.Getpid() || result.ResourceConflict.Worktree != f.inst.Worktree {
			t.Errorf("missing worktree owner identity: %+v", result.ResourceConflict)
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			t.Errorf("expected exactly one JSON result, found trailing output: %v", err)
		}
	}
	if f.project.configured.Load() != 0 || f.project.executed.Load() != 0 || f.project.fingerprint.Load() != 0 {
		t.Errorf("rejected command invoked adapter callbacks: configure=%d execute=%d fingerprint=%d", f.project.configured.Load(), f.project.executed.Load(), f.project.fingerprint.Load())
	}
	for path, before := range f.before {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("rejected command removed owner file %s: %v", path, err)
			continue
		}
		if !bytes.Equal(before, after) {
			t.Errorf("rejected command changed owner file %s", path)
		}
	}
}
