package instance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

func TestRecordDaemonDoesNotRewriteExecutionState(t *testing.T) {
	inst := newDaemonStateInstance(t)
	inst.Env["EXECUTION_VALUE"] = "preserve"
	inst.Processes["service"] = api.ProcessRef{PID: 4321}
	if err := Save(inst); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(instancePath(inst.Worktree, inst.ID), "instance.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	// A daemon control update must not depend on publishing runtime.env.
	envPath := filepath.Join(instancePath(inst.Worktree, inst.ID), "runtime.env")
	if err := os.Remove(envPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(envPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RecordDaemon(inst, 7654, "daemon.log"); err != nil {
		t.Fatalf("daemon metadata update touched execution environment: %v", err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("daemon metadata update rewrote the execution snapshot")
	}
}

func TestLoadDaemonDoesNotReadExecutionSnapshot(t *testing.T) {
	inst := newDaemonStateInstance(t)
	path := filepath.Join(instancePath(inst.Worktree, inst.ID), "instance.json")
	if err := os.WriteFile(path, []byte(`{"id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := LoadDaemon(inst.Worktree, inst.ID)
	if err != nil || ref.PID != 0 {
		t.Fatalf("absent daemon control depended on execution snapshot: %+v, %v", ref, err)
	}
}

func TestDaemonControlSurvivesStaleExecutionSnapshot(t *testing.T) {
	inst := newDaemonStateInstance(t)
	stale := *inst
	if err := RecordDaemon(inst, 7654, "daemon.log"); err != nil {
		t.Fatal(err)
	}
	if err := Save(&stale); err != nil {
		t.Fatal(err)
	}
	for _, resolve := range []bool{false, true} {
		var loaded *api.Instance
		var err error
		if resolve {
			loaded, err = Resolve(inst.Worktree, "test")
		} else {
			loaded, err = Load(inst.Worktree, inst.ID)
		}
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Daemon.PID != 7654 || loaded.Daemon.LogPath != "daemon.log" {
			t.Errorf("resolve=%t: stale execution snapshot hid daemon metadata: %+v", resolve, loaded.Daemon)
		}
	}
}

func TestClearedDaemonControlCannotBeResurrectedBySnapshot(t *testing.T) {
	inst := newDaemonStateInstance(t)
	if err := RecordDaemon(inst, 7654, "daemon.log"); err != nil {
		t.Fatal(err)
	}
	stale, err := Load(inst.Worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ClearDaemon(inst); err != nil {
		t.Fatal(err)
	}
	if err := Save(stale); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(inst.Worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Daemon.PID != 0 {
		t.Fatalf("stale execution snapshot resurrected cleared daemon: %+v", loaded.Daemon)
	}
	if _, err := os.Stat(filepath.Join(instancePath(inst.Worktree, inst.ID), "daemon.json")); !os.IsNotExist(err) {
		t.Fatalf("cleared daemon record remains: %v", err)
	}
}

func TestRecordDaemonBeforeExecutionCreatesOnlyControlState(t *testing.T) {
	worktree := t.TempDir()
	id, root, err := IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordDaemon(&api.Instance{ID: id, Worktree: root}, 7654, "daemon.log"); err != nil {
		t.Fatal(err)
	}
	ref, err := LoadDaemon(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if ref.PID != 7654 || ref.LogPath != "daemon.log" {
		t.Fatalf("control-only daemon was not readable: %+v", ref)
	}
	for _, name := range []string{"instance.json", "runtime.env", "status.json"} {
		if _, err := os.Stat(filepath.Join(instancePath(root, id), name)); !os.IsNotExist(err) {
			t.Errorf("daemon startup created execution state %s: %v", name, err)
		}
	}
	path := filepath.Join(instancePath(root, id), "daemon.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("daemon control permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestLoadRejectsCorruptDaemonControl(t *testing.T) {
	inst := newDaemonStateInstance(t)
	path := filepath.Join(instancePath(inst.Worktree, inst.ID), "daemon.json")
	if err := os.WriteFile(path, []byte(`{"pid":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(inst.Worktree, inst.ID); err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Errorf("Load ignored invalid daemon control: %v", err)
	}
	if _, err := Resolve(inst.Worktree, "replacement"); err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Errorf("Resolve ignored invalid daemon control: %v", err)
	}
	if _, err := LoadDaemon(inst.Worktree, inst.ID); err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Errorf("LoadDaemon ignored invalid daemon control: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"pid":` {
		t.Fatalf("invalid daemon control was overwritten: %q", data)
	}
}

func newDaemonStateInstance(t *testing.T) *api.Instance {
	t.Helper()
	cacheRoot := t.TempDir()
	t.Setenv("HOME", cacheRoot)
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("LOCALAPPDATA", cacheRoot)
	inst, err := Resolve(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestStopDaemonWorkPreservesDaemonControl(t *testing.T) {
	inst := newDaemonStateInstance(t)
	if err := RecordDaemon(inst, os.Getpid(), "daemon.log"); err != nil {
		t.Fatal(err)
	}
	inst.Processes["exited-service"] = api.ProcessRef{PID: 1 << 30}
	if err := Save(inst); err != nil {
		t.Fatal(err)
	}
	if _, err := StopDaemonWork(inst, nil, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	ref, err := LoadDaemon(inst.Worktree, inst.ID)
	if err != nil || ref.PID != os.Getpid() {
		t.Fatalf("work cleanup changed owning daemon: %+v, %v", ref, err)
	}
	loaded, err := Load(inst.Worktree, inst.ID)
	if err != nil || len(loaded.Processes) != 0 {
		t.Fatalf("work cleanup retained stopped processes: %+v, %v", loaded, err)
	}
}

func TestExecutionSnapshotOmitsDaemonMetadata(t *testing.T) {
	inst := newDaemonStateInstance(t)
	if err := RecordDaemon(inst, 7654, "daemon.log"); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(inst.Worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(loaded); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(instancePath(inst.Worktree, inst.ID), "instance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if _, exists := snapshot["daemon"]; exists {
		t.Fatal("execution snapshot embedded separately owned daemon metadata")
	}
}

func TestStopDaemonWorkNeverStopsDaemonThroughAliases(t *testing.T) {
	inst := newDaemonStateInstance(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestInstanceDaemonPIDHelper$")
	cmd.Env = append(os.Environ(), "DEVFLOW_TEST_DAEMON_PID_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan bool, 1)
	go func() { scanner := bufio.NewScanner(stdout); ready <- scanner.Scan() && scanner.Text() == "ready" }()
	select {
	case ok := <-ready:
		if !ok {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("daemon helper did not start")
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("daemon helper did not become ready")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon helper did not exit")
		}
	})
	pid := cmd.Process.Pid
	inst.Daemon = api.DaemonRef{PID: pid}
	inst.Processes["daemon-alias"] = api.ProcessRef{PID: pid}
	if err := Save(inst); err != nil {
		t.Fatal(err)
	}
	stopped, err := StopDaemonWork(inst, map[string]int{"status-alias": pid}, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 0 || !ProcessAlive(pid) {
		t.Fatalf("stop work terminated owning daemon through an alias: stopped=%v alive=%t", stopped, ProcessAlive(pid))
	}
}

func TestInstanceDaemonPIDHelper(t *testing.T) {
	if os.Getenv("DEVFLOW_TEST_DAEMON_PID_HELPER") != "1" {
		return
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
