package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/instance"
)

func TestStopDiagnosticsPrecedeCleanupAndCorrelateShutdown(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "diagnostic-test")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{worktree: worktree, instanceID: inst.ID, logPath: filepath.Join(worktree, "daemon.log"), shutdown: make(chan struct{})}
	s.transitionMu.Lock()
	unlock := sync.OnceFunc(s.transitionMu.Unlock)
	defer unlock()

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleConn(ctx, serverConn)
	}()
	defer func() { unlock(); cancel(); <-done }()
	var req Request
	if err := json.Unmarshal([]byte(`{"id":"tui-stop-test","action":"stop","all":true,"callerPid":12345}`), &req); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(clientConn).Encode(req); err != nil {
		t.Fatal(err)
	}
	if !waitForDaemonCondition(2*time.Second, func() bool {
		data, _ := os.ReadFile(s.logPath)
		return strings.Contains(string(data), "event=daemon_stop_requested")
	}) {
		t.Fatal("stop request was not logged before waiting for the cleanup transition lock")
	}
	before, err := os.ReadFile(s.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "event=daemon_stop_completed") {
		t.Fatal("cleanup reported completion while still blocked")
	}
	unlock()
	var response frame
	if err := json.NewDecoder(clientConn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Response == nil || !response.Response.OK {
		t.Fatalf("stop failed: %+v", response)
	}
	if err := json.NewEncoder(clientConn).Encode(frame{Type: responseAckFrameType, ID: req.ID}); err != nil {
		t.Fatal(err)
	}
	<-done
	data, err := os.ReadFile(s.logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"event=daemon_stop_requested", "event=daemon_stop_completed", "event=daemon_shutdown_requested"}
	if len(lines) != len(want) {
		t.Fatalf("unexpected stop evidence: %s", data)
	}
	for i, event := range want {
		if !strings.Contains(lines[i], event) || !strings.Contains(lines[i], `request_id="tui-stop-test"`) || !strings.Contains(lines[i], "caller_pid=12345") {
			t.Errorf("uncorrelated or reordered stop evidence: %s", lines[i])
		}
	}
	if !strings.Contains(lines[0], "caller_source=request") || !strings.Contains(lines[1], "status=ok") {
		t.Errorf("caller provenance or cleanup outcome missing: %s", data)
	}
}

func TestStopDiagnosticsRetainFailureWithoutShutdownAndExcludePreview(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "diagnostic-test")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{worktree: worktree, instanceID: inst.ID, logPath: filepath.Join(worktree, "daemon.log"), shutdown: make(chan struct{})}
	resp := s.handleRequest(context.Background(), Request{ID: "failed-stop", Action: ActionStop})
	if resp.OK || resp.Error == nil {
		t.Fatalf("invalid scoped stop unexpectedly succeeded: %+v", resp)
	}
	data, err := os.ReadFile(s.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "event=daemon_stop_completed") || !strings.Contains(string(data), "status=failed") || strings.Contains(string(data), "event=daemon_shutdown_requested") {
		t.Fatalf("failed cleanup evidence misstates outcome: %s", data)
	}
	_ = s.handleRequest(context.Background(), Request{ID: "preview-stop", Action: ActionStop, All: true, Preview: true})
	after, err := os.ReadFile(s.logPath)
	if err != nil || string(after) != string(data) {
		t.Fatalf("read-only preview wrote exit diagnostics: %q, %v", after, err)
	}
}

func TestClientCallAddsCallerPID(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("df-caller-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)
	callerPID := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			callerPID <- -1
			return
		}
		defer conn.Close()
		var request struct {
			ID        string `json:"id"`
			CallerPID int    `json:"callerPid"`
		}
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			callerPID <- -1
			return
		}
		callerPID <- request.CallerPID
		_ = json.NewEncoder(conn).Encode(frame{Type: responseFrameType, ID: request.ID, Response: &Response{ID: request.ID, OK: true}})
		var ack frame
		_ = json.NewDecoder(conn).Decode(&ack)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := (&Client{socketPath: socketPath}).Call(ctx, Request{Action: ActionPing}); err != nil {
		t.Fatal(err)
	}
	if got := <-callerPID; got != os.Getpid() {
		t.Fatalf("caller PID = %d, want process PID %d", got, os.Getpid())
	}
	<-done
}

func TestDaemonDiagnosticWriteFailureIsVisibleAndPreservesStopResult(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "diagnostic-test")
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	originalStderr := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = originalStderr }()
	// A directory cannot be opened as the log file; cleanup must still run.
	s := &Server{worktree: worktree, instanceID: inst.ID, logPath: worktree}
	resp := s.handleRequest(context.Background(), Request{ID: "unwritable-log", Action: ActionStop, All: true})
	_ = writer.Close()
	data, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(data), "daemon diagnostic write failed") {
		t.Fatalf("diagnostic storage failure was hidden: %q, %v", data, err)
	}
	if !resp.OK || resp.Stop == nil || resp.Error != nil {
		t.Fatalf("diagnostic failure replaced successful cleanup: %+v", resp)
	}
}
