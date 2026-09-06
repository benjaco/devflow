package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/daemon"
	"github.com/benjaco/devflow/pkg/instance"
)

func TestCompiledJSONReportsDisconnectedDaemon(t *testing.T) {
	isolateJSONContractState(t)
	worktree := t.TempDir()
	startJSONContractPeer(t, worktree, func(req daemon.Request, conn net.Conn) {
		// Ping succeeds, but execution loses the connection before a result.
		// This proves the CLI labels a transport failure without parsing prose.
		if req.Action == daemon.ActionPing {
			writeJSONContractPeerResponse(conn, req.ID, map[string]any{"ok": true})
		}
	})
	var stdout, stderr bytes.Buffer
	err := runJSONContractLocal(t, worktree, &stdout, &stderr, "run", "fail", "--json")
	assertJSONContractFailure(t, stdout.String(), stderr.String(), err, "daemon_unavailable", "transport")
}

func TestCompiledActionJSONPreservesPositionalInputsAroundFlags(t *testing.T) {
	isolateJSONContractState(t)
	worktree := t.TempDir()
	startJSONContractPeer(t, worktree, func(req daemon.Request, conn net.Conn) {
		switch req.Action {
		case daemon.ActionPing:
			writeJSONContractPeerResponse(conn, req.ID, map[string]any{"ok": true})
		case daemon.ActionRunAction:
			writeJSONContractPeerResponse(conn, req.ID, map[string]any{
				"ok":           true,
				"actionResult": daemon.ActionRunResult{ActionID: req.ActionID, Inputs: req.Inputs, Status: "completed"},
			})
		}
	})
	for _, args := range [][]string{
		{"action", "run", "create", "migration", "--json"},
		{"action", "run", "--json", "create", "migration"},
	} {
		var stdout, stderr bytes.Buffer
		if err := runJSONContractLocal(t, worktree, &stdout, &stderr, args...); err != nil {
			t.Fatalf("action %q failed: %v\nstdout=%s\nstderr=%s", args, err, &stdout, &stderr)
		}
		var result daemon.ActionRunResult
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("action result is not one JSON document: %v\nstdout=%s", err, &stdout)
		}
		if result.ActionID != "create" || result.Inputs["name"] != "migration" {
			t.Fatalf("action %q changed positional inputs: %+v", args, result)
		}
	}
	var stdout, stderr bytes.Buffer
	err := runJSONContractLocal(t, worktree, &stdout, &stderr, "action", "run", "create", "migration", "extra", "--json")
	assertJSONContractFailure(t, stdout.String(), stderr.String(), err, "invalid_arguments", "parsing")
}

func TestCompiledWatchJSONIsJSONLThroughTransportFailure(t *testing.T) {
	isolateJSONContractState(t)
	worktree := t.TempDir()
	eventObserved := make(chan struct{})
	var observedOnce sync.Once
	startJSONContractPeer(t, worktree, func(req daemon.Request, conn net.Conn) {
		switch req.Action {
		case daemon.ActionPing:
			writeJSONContractPeerResponse(conn, req.ID, map[string]any{"ok": true})
		case daemon.ActionWatch:
			writeJSONContractPeerResponse(conn, req.ID, map[string]any{
				"ok":      true,
				"started": daemon.StartResult{Target: "watch", Mode: api.ModeWatch, Accepted: true},
			})
		case daemon.ActionSubscribe:
			_ = json.NewEncoder(conn).Encode(map[string]any{
				"type":  "event",
				"event": api.Event{Type: api.EventLogLine, Task: "observe", Line: "watch-evidence-marker"},
			})
			// Wait until the child actually emits the event before closing the
			// socket: Windows may discard buffered bytes on immediate close.
			select {
			case <-eventObserved:
			case <-time.After(10 * time.Second):
			}
		}
	})
	stdout := &jsonContractObservedWriter{observed: func(text string) {
		if strings.Contains(text, "watch-evidence-marker") {
			observedOnce.Do(func() { close(eventObserved) })
		}
	}}
	var stderr bytes.Buffer
	err := runJSONContractLocal(t, worktree, stdout, &stderr, "watch", "watch", "--json")
	if err == nil {
		t.Fatalf("closed subscription must fail: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want start, event, and terminal error JSONL records, got %d:\n%s\nstderr=%s", len(lines), stdout.String(), stderr.String())
	}
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("stream record %d is not JSON: %q", i, line)
		}
	}
	var started daemon.StartResult
	if err := json.Unmarshal([]byte(lines[0]), &started); err != nil || started.Target != "watch" || !started.Accepted {
		t.Fatalf("start record=%q decode=%v", lines[0], err)
	}
	var event api.Event
	if err := json.Unmarshal([]byte(lines[1]), &event); err != nil || event.Type != api.EventLogLine || event.Line != "watch-evidence-marker" {
		t.Fatalf("event record=%q decode=%v", lines[1], err)
	}
	assertJSONContractFailure(t, lines[2], stderr.String(), err, "daemon_unavailable", "transport")
}

type jsonContractObservedWriter struct {
	buffer   bytes.Buffer
	observed func(string)
}

func (w *jsonContractObservedWriter) Write(data []byte) (int, error) {
	n, err := w.buffer.Write(data)
	w.observed(w.buffer.String())
	return n, err
}

func (w *jsonContractObservedWriter) String() string { return w.buffer.String() }

func startJSONContractPeer(t *testing.T, worktree string, respond func(daemon.Request, net.Conn)) {
	t.Helper()
	writeLocalProjectFile(t, worktree, jsonContractProjectSource)
	if stdout, stderr, err := runJSONContractCommand(t, worktree, "graph", "list", "--json"); err != nil {
		t.Fatalf("build fixture adapter: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	localBinary := localProjectBinaryPathForTest(worktree)
	if err := fsutil.CopyFile(localBinary, filepath.Join(worktree, ".devflow", "bin", "devflow-daemon"+testExeSuffix())); err != nil {
		t.Fatal(err)
	}
	id, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	path, err := instance.DaemonSocketPath(id)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
			var req daemon.Request
			if err := json.NewDecoder(conn).Decode(&req); err == nil {
				respond(req, conn)
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
		_ = os.Remove(path)
	})
}

func writeJSONContractPeerResponse(conn net.Conn, id string, response map[string]any) {
	response["id"] = id
	if err := json.NewEncoder(conn).Encode(map[string]any{"type": "response", "response": response}); err != nil {
		return
	}
	var ack map[string]any
	_ = json.NewDecoder(conn).Decode(&ack)
}

func runJSONContractLocal(t *testing.T, worktree string, stdout, stderr io.Writer, args ...string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, localProjectBinaryPathForTest(worktree), args...)
	cmd.Dir = worktree
	cmd.Env = withEnv(os.Environ(), envLocalExec, "1")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("local command %q exceeded deadline", args)
	}
	return err
}
