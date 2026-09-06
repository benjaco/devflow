package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func TestPublishPersistsAndFansOutEvents(t *testing.T) {
	worktree := t.TempDir()
	instanceID, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		worktree:    worktree,
		instanceID:  instanceID,
		subscribers: map[chan api.Event]bool{},
	}
	ch := s.addSubscriber()
	defer s.removeSubscriber(ch)

	first := api.Event{Type: api.EventRunStarted, InstanceID: instanceID, Target: "fullstack"}
	second := api.Event{
		Type:          api.EventWatchCycleStart,
		InstanceID:    instanceID,
		Files:         []string{"frontend/src/page.tsx"},
		AffectedTasks: []string{"frontend_dev"},
	}
	s.publish(first)
	s.publish(second)

	for _, want := range []api.Event{first, second} {
		select {
		case got := <-ch:
			if got.Type != want.Type {
				t.Fatalf("unexpected event type %q, want %q", got.Type, want.Type)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for fanout event")
		}
	}

	data, err := os.ReadFile(instance.EventsPath(worktree, instanceID))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected 2 persisted event lines, got %d", len(lines))
	}
	var payload map[string]any
	if err := json.Unmarshal(lines[1], &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload["type"]; got != string(api.EventWatchCycleStart) {
		t.Fatalf("unexpected event type %v", got)
	}
	affected, ok := payload["affectedTasks"].([]any)
	if !ok || len(affected) != 1 || affected[0] != "frontend_dev" {
		t.Fatalf("unexpected affectedTasks payload: %v", payload["affectedTasks"])
	}
}

func TestWaitForFlushAckDoesNotRewriteSyncSentinel(t *testing.T) {
	s, active := newFlushGenerationFixture(t)
	close(active.watchReady)
	requestID := "flush-observed"
	syncPath := instance.FlushSyncPath(s.worktree, s.instanceID, requestID)
	if err := os.MkdirAll(filepath.Dir(syncPath), 0o755); err != nil {
		t.Fatal(err)
	}
	contents := []byte(requestID + "\n")
	if err := os.WriteFile(syncPath, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	writeDone := make(chan struct{})
	t.Cleanup(func() { <-writeDone })
	go func() {
		defer close(writeDone)
		time.Sleep(150 * time.Millisecond)
		written <- instance.WriteFlushAck(s.worktree, s.instanceID, api.FlushResult{
			RequestID: requestID, InstanceID: s.instanceID, Worktree: s.worktree,
			Target: active.target, Synced: true, Success: true,
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := s.waitForFlushAck(ctx, active, requestID)
	if err != nil || !result.Success || !result.Synced {
		t.Fatalf("expected successful ack, result=%+v err=%v", result, err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(syncPath)
	if err != nil || !bytes.Equal(data, contents) {
		t.Fatalf("ack wait rewrote the sync sentinel: contents=%q err=%v", data, err)
	}
}

func TestFlushRequestWriteFailureReturnsStructuredContext(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	const projectName = "daemon-flush-write-error"
	project.Register(daemonTestProject{
		name: projectName,
		tasks: []project.Task{{
			Name: "noop",
			Kind: project.KindOnce,
			Run: func(context.Context, *project.Runtime) error {
				return nil
			},
		}},
		targets: []project.Target{{Name: "up", RootTasks: []string{"noop"}}},
	})
	flushRoot := instance.FlushRoot(worktree, inst.ID)
	if err := os.MkdirAll(filepath.Dir(flushRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flushRoot, []byte("blocks the flush directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(worktree, "daemon.log")
	s := &Server{
		worktree:    worktree,
		instanceID:  inst.ID,
		projectName: projectName,
		logPath:     logPath,
		active: &activeRun{
			projectName: projectName,
			target:      "up",
			mode:        api.ModeWatch,
			done:        make(chan struct{}),
			watchReady:  make(chan struct{}),
		},
	}

	close(s.active.watchReady)

	result, err := s.flush(context.Background(), projectName, "up", time.Second, 1)
	if err == nil || !strings.Contains(err.Error(), "flush failed") {
		t.Fatalf("expected contextual flush failure, got %v", err)
	}
	if result.RequestID == "" || result.Worktree != worktree || result.InstanceID != inst.ID || result.Project != projectName || result.Target != "up" || result.Mode != api.ModeWatch || result.UpdatedAt.IsZero() {
		t.Fatalf("flush failure lost request context: %+v", result)
	}
	if len(result.Issues) != 1 || result.Issues[0].Kind != "request_write_error" || result.Issues[0].Message == "" || result.Issues[0].LogPath != logPath {
		t.Fatalf("flush failure lost the underlying issue: %+v", result.Issues)
	}
}

func TestFlushStartsWatchAfterCompletedDetachedRun(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	const projectName = "daemon-flush-completed-detached-run"
	var runs atomic.Int32
	project.Register(daemonTestProject{
		name: projectName,
		tasks: []project.Task{{
			Name: "check",
			Kind: project.KindOnce,
			Run: func(context.Context, *project.Runtime) error {
				runs.Add(1)
				return nil
			},
		}},
		targets: []project.Target{{Name: "up", RootTasks: []string{"check"}}},
	})
	inst.LastRun = api.RunConfig{Project: projectName, Target: "up", Mode: api.ModeDev, Detached: true}
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(worktree, "daemon.log")
	if err := instance.RecordDaemon(inst, os.Getpid(), logPath); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		worktree:    worktree,
		instanceID:  inst.ID,
		projectName: projectName,
		logPath:     logPath,
		subscribers: map[chan api.Event]bool{},
	}
	defer s.stopActive(3 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := s.flush(ctx, projectName, "up", 4*time.Second, 1)
	if err != nil || !result.Started || !result.Success || runs.Load() != 1 {
		t.Fatalf("completed run prevented a fresh watch: result=%+v runs=%d err=%v", result, runs.Load(), err)
	}
}

func TestEnsureSerializesDaemonStartup(t *testing.T) {
	worktree := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var starts atomic.Int32
	restore := SetStartDaemonFuncForTest(func(worktree, instanceID, projectName string) error {
		if starts.Add(1) > 1 {
			return nil
		}
		go func() {
			_ = Serve(ctx, Options{
				Worktree: worktree,
				Project:  projectName,
				LogPath:  filepath.Join(worktree, ".devflow", "logs", instanceID, "daemon.log"),
			})
		}()
		return nil
	})
	defer restore()

	const callers = 6
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, _, err := Ensure(callCtx, worktree, "")
			if err == nil {
				err = client.Ping(callCtx)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("expected one daemon start, got %d", got)
	}
}

func TestEnsureCreatesMissingDaemonLogForLiveDaemon(t *testing.T) {
	worktree := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var starts atomic.Int32
	restore := SetStartDaemonFuncForTest(func(worktree, instanceID, projectName string) error {
		starts.Add(1)
		go func() {
			_ = Serve(ctx, Options{
				Worktree: worktree,
				Project:  projectName,
				LogPath:  filepath.Join(worktree, ".devflow", "logs", instanceID, "daemon.log"),
			})
		}()
		return nil
	})
	defer restore()

	callCtx, cancelCall := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCall()
	client, _, err := Ensure(callCtx, worktree, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(callCtx); err != nil {
		t.Fatal(err)
	}
	id, realWorktree, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(realWorktree, ".devflow", "logs", id, "daemon.log")
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}

	client, _, err = Ensure(callCtx, worktree, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(callCtx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("expected Ensure to recreate missing daemon log: %v", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("expected existing daemon to be reused, got %d starts", got)
	}
}

func TestRunProjectActionExecutesTaskBackedAction(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	const projectName = "daemon-action-run"
	project.Register(project.Define(func(ctx context.Context, b *project.Builder) error {
		b.Name(projectName)
		task := b.Task("write_action_input").
			NoCache().
			Run(func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				return os.WriteFile(filepath.Join(rt.Worktree, "action.txt"), []byte(rt.Env["THING_NAME"]), 0o644)
			})
		b.Target("up", task)
		b.Action("thing.create").
			Kind("devflow.test.create").
			Component("thing").
			Task(task).
			Input(project.ActionInput{Name: "name", Required: true, Env: "THING_NAME"}).
			Writes("action.txt")
		return nil
	}))

	s := &Server{
		worktree:    worktree,
		instanceID:  inst.ID,
		projectName: projectName,
		subscribers: map[chan api.Event]bool{},
		shutdown:    make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := s.runProjectAction(ctx, projectName, "thing.create", "", "", map[string]string{"name": "alpha"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Status != "succeeded" || result.ActionID != "thing.create" {
		t.Fatalf("unexpected action result: %+v", result)
	}
	if len(result.CreatedFiles) != 1 || result.CreatedFiles[0] != "action.txt" {
		t.Fatalf("unexpected created files: %+v", result.CreatedFiles)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "action.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "alpha" {
		t.Fatalf("unexpected action file content %q", string(data))
	}
}

func TestSubscribeReturnsWhenContextCanceled(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), "df-subscribe-"+time.Now().Format("150405.000000000")+".sock")
	defer os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connected := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req Request
		_ = json.NewDecoder(conn).Decode(&req)
		close(connected)
		_, _ = conn.Read(make([]byte, 1))
	}()

	client := &Client{socketPath: socketPath}
	subCtx, cancelSub := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- client.Subscribe(subCtx, func(api.Event) {})
	}()
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("Subscribe did not connect to test daemon")
	}
	cancelSub()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Subscribe returned error after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Subscribe did not return after context cancellation")
	}
}

func TestClientCallReturnsWhenContextCanceledWithoutDeadline(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), "df-call-cancel-"+time.Now().Format("150405.000000000")+".sock")
	defer os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connected := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		var req Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			_ = conn.Close()
			return
		}
		connected <- conn
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (&Client{socketPath: socketPath}).Call(ctx, Request{Action: ActionPing})
		done <- err
	}()
	select {
	case conn := <-connected:
		defer conn.Close()
	case <-time.After(time.Second):
		t.Fatal("Call did not send its request to the test daemon")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Call cancellation error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Call did not return after context cancellation")
	}
}

func TestHandleConnReturnsWhenCanceledBeforeRequest(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Server{}).handleConn(ctx, serverConn)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection handler remained blocked on an unread request after cancellation")
	}
}

func TestHandleConnRemovesIdleSubscriberAfterDisconnect(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{subscribers: map[chan api.Event]bool{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleConn(ctx, serverConn)
	}()
	if err := json.NewEncoder(clientConn).Encode(Request{Action: ActionSubscribe}); err != nil {
		t.Fatal(err)
	}
	if !waitForDaemonCondition(time.Second, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.subscribers) == 1
	}) {
		t.Fatal("connection handler did not register the subscription")
	}
	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle subscriber remained registered after the client disconnected")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.subscribers) != 0 {
		t.Fatalf("disconnected subscribers retained: %d", len(s.subscribers))
	}
}

func TestClientCallAcknowledgesFinalErrorResponse(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), "df-response-ack-"+time.Now().Format("150405.000000000")+".sock")
	defer os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		dec := json.NewDecoder(conn)
		enc := json.NewEncoder(conn)
		var req Request
		if err := dec.Decode(&req); err != nil {
			serverDone <- err
			return
		}
		result := api.FlushResult{
			RequestID:  "flush-1",
			InstanceID: "instance-1",
			Worktree:   `C:\worktree`,
			Target:     "build",
			Mode:       api.ModeWatch,
			Issues: []api.FlushIssue{{
				Kind:    "target_mismatch",
				Message: `live watch target is "gen", requested "build"`,
			}},
		}
		resp := Response{ID: req.ID, OK: false, Error: &api.CommandError{Code: "flush_failed", Phase: "execution", Message: "flush failed"}, Flush: &result}
		if err := enc.Encode(frame{Type: responseFrameType, ID: req.ID, Response: &resp}); err != nil {
			serverDone <- err
			return
		}
		var ack frame
		if err := dec.Decode(&ack); err != nil {
			serverDone <- fmt.Errorf("read response acknowledgment: %w", err)
			return
		}
		if ack.Type != responseAckFrameType || ack.ID != req.ID {
			serverDone <- fmt.Errorf("unexpected response acknowledgment: %+v", ack)
			return
		}
		serverDone <- nil
	}()

	client := &Client{socketPath: socketPath}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, callErr := client.Call(ctx, Request{Action: ActionFlush})
	if callErr == nil || !strings.Contains(callErr.Error(), "flush failed") {
		t.Fatalf("expected flush error, got %v", callErr)
	}
	if resp.Flush == nil || len(resp.Flush.Issues) != 1 || resp.Flush.Issues[0].Kind != "target_mismatch" {
		t.Fatalf("error response lost structured flush result: %+v", resp)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client did not acknowledge the final daemon response")
	}
}

func TestStartActiveReusesExistingMatchingWatch(t *testing.T) {
	name := "daemon-idempotent-watch"
	project.Register(daemonTestProject{
		name:    name,
		tasks:   []project.Task{{Name: "serve", Kind: project.KindService}},
		targets: []project.Target{{Name: "up", RootTasks: []string{"serve"}}},
	})
	active := &activeRun{
		projectName: name,
		target:      "up",
		mode:        api.ModeWatch,
		done:        make(chan struct{}),
	}
	s := &Server{
		worktree:   t.TempDir(),
		instanceID: "abc123",
		logPath:    filepath.Join(t.TempDir(), "daemon.log"),
		active:     active,
	}
	started, err := s.startActive(context.Background(), name, "up", api.ModeWatch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if started.Target != "up" || started.Mode != api.ModeWatch {
		t.Fatalf("unexpected start result: %+v", started)
	}
	if s.active != active {
		t.Fatal("expected matching active watch to be reused")
	}
}

func TestDaemonNeedsRestartWhenCopiedExecutableIsMissingOrStale(t *testing.T) {
	worktree := t.TempDir()
	if !daemonNeedsRestart(worktree, "abc123") {
		t.Fatal("expected restart when daemon executable is missing")
	}
	if err := os.MkdirAll(filepath.Dir(daemonExecutablePath(worktree)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemonExecutablePath(worktree), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !daemonNeedsRestart(worktree, "abc123") {
		t.Fatal("expected restart when daemon executable is stale")
	}
	current, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := copyFileForDaemonTest(current, daemonExecutablePath(worktree)); err != nil {
		t.Fatal(err)
	}
	if daemonNeedsRestart(worktree, "abc123") {
		t.Fatal("did not expect restart when daemon executable matches current executable")
	}
}

func TestStopAllUsesRecordedTaskOwnershipOnly(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	trackedPID, orphanPID, logPID := os.Getpid()+1, os.Getpid()+2, os.Getpid()+3
	inst.Processes["tracked"] = api.ProcessRef{PID: trackedPID}
	if err := instance.SaveStatus(worktree, inst.ID, "dev", api.ModeWatch, map[string]api.NodeStatus{
		"tracked": {Name: "tracked", PID: trackedPID},
		"orphan":  {Name: "orphan", PID: orphanPID},
	}); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(worktree, ".devflow", "logs", inst.ID, "supervisor.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, fmt.Appendf(nil, "child pid=%d\n", logPID), 0o600); err != nil {
		t.Fatal(err)
	}
	refs := additionalRecordedProcessRefs(worktree, inst.ID, inst)
	if len(refs) != 1 || refs["orphan"] != orphanPID {
		t.Fatalf("stop scope must contain only additional recorded task PIDs, got %v", refs)
	}
}

func TestDownstreamInvalidateTasksReturnsCacheableAndStampedOnceTasksInTargetClosure(t *testing.T) {
	g, err := graph.New([]project.Task{
		{Name: "a", Kind: project.KindOnce, Cache: true},
		{Name: "b", Kind: project.KindOnce, Cache: true, Deps: []string{"a"}},
		{Name: "c", Kind: project.KindService, Deps: []string{"b"}},
		{Name: "d", Kind: project.KindOnce, Cache: false, Deps: []string{"b"}},
		{Name: "e", Kind: project.KindOnce, Cache: true, Deps: []string{"d"}},
		{Name: "f", Kind: project.KindOnce, Stamp: true, Deps: []string{"b"}},
	}, []project.Target{
		{Name: "main", RootTasks: []string{"c", "e", "f"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	names, err := downstreamInvalidateTasks(g, "main", "b")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names, ",")
	want := "b,e,f"
	if got != want {
		t.Fatalf("unexpected invalidate tasks: got %q want %q", got, want)
	}
}

func TestDownstreamInvalidateTasksForGroupReturnsItsCacheableInputs(t *testing.T) {
	g, err := graph.New([]project.Task{
		{Name: "build_a", Kind: project.KindOnce, Cache: true},
		{Name: "build_b", Kind: project.KindOnce, Cache: true},
		{Name: "aggregate", Kind: project.KindGroup, Deps: []string{"build_a", "build_b"}},
		{Name: "serve", Kind: project.KindService, Deps: []string{"aggregate"}},
	}, []project.Target{
		{Name: "main", RootTasks: []string{"serve"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	names, err := downstreamInvalidateTasks(g, "main", "aggregate")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names, ",")
	want := "build_a,build_b"
	if got != want {
		t.Fatalf("unexpected invalidate tasks for group: got %q want %q", got, want)
	}
}

func TestExecutionGraphForProjectResolvesTaskTargets(t *testing.T) {
	p := daemonTestProject{
		name: "daemon-execution-graph",
		tasks: []project.Task{
			{Name: "build", Kind: project.KindOnce, Cache: true},
			{Name: "serve", Kind: project.KindService, Deps: []string{"build"}},
		},
		targets: []project.Target{
			{Name: "fullstack", RootTasks: []string{"serve"}},
		},
	}
	g, target, err := executionGraphForProject(p, "build")
	if err != nil {
		t.Fatal(err)
	}
	if target != "build" {
		t.Fatalf("expected resolved synthetic target to be build, got %q", target)
	}
	closure, err := g.TargetClosure(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(closure, ","); got != "build" {
		t.Fatalf("unexpected synthetic target closure: %q", got)
	}
}

func TestWriteInvalidateTransitionMarksDirtyAndPendingNodes(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]api.NodeStatus{
		"build_a":   {Name: "build_a", Kind: "once", State: api.StateCached, LastRunKey: "a"},
		"build_b":   {Name: "build_b", Kind: "once", State: api.StateCached, LastRunKey: "b"},
		"aggregate": {Name: "aggregate", Kind: "group", State: api.StateDone},
		"serve":     {Name: "serve", Kind: "service", State: api.StateRunning, PID: 123},
	}
	if err := instance.SaveStatus(worktree, inst.ID, "main", api.ModeDev, nodes); err != nil {
		t.Fatal(err)
	}
	g, err := graph.New([]project.Task{
		{Name: "build_a", Kind: project.KindOnce, Cache: true},
		{Name: "build_b", Kind: project.KindOnce, Cache: true},
		{Name: "aggregate", Kind: project.KindGroup, Deps: []string{"build_a", "build_b"}},
		{Name: "serve", Kind: project.KindService, Deps: []string{"aggregate"}},
	}, []project.Target{{Name: "main", RootTasks: []string{"serve"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeInvalidateTransition(worktree, inst.ID, "main", g, []string{"build_a", "build_b"}); err != nil {
		t.Fatal(err)
	}
	state, err := instance.LoadStatus(worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Nodes["build_a"].State != api.StateDirty || state.Nodes["build_a"].LastRunKey != "" {
		t.Fatalf("expected build_a to become dirty without key, got %+v", state.Nodes["build_a"])
	}
	if state.Nodes["aggregate"].State != api.StatePending {
		t.Fatalf("expected aggregate to become pending, got %+v", state.Nodes["aggregate"])
	}
	if state.Nodes["serve"].State != api.StatePending || state.Nodes["serve"].PID != 0 {
		t.Fatalf("expected serve to become pending without pid, got %+v", state.Nodes["serve"])
	}
}

type daemonLifecycleHandle struct {
	done    chan struct{}
	stop    sync.Once
	stopped atomic.Bool
}

func newDaemonLifecycleHandle() *daemonLifecycleHandle {
	return &daemonLifecycleHandle{done: make(chan struct{})}
}
func (h *daemonLifecycleHandle) PID() int    { return 0 }
func (h *daemonLifecycleHandle) Alive() bool { return !h.stopped.Load() }
func (h *daemonLifecycleHandle) Wait() error {
	<-h.done
	return nil
}
func (h *daemonLifecycleHandle) Stop() error {
	h.stop.Do(func() {
		h.stopped.Store(true)
		close(h.done)
	})
	return nil
}

type daemonLifecycleProject struct {
	name    string
	mu      sync.Mutex
	handles map[string][]*daemonLifecycleHandle
}

func (p *daemonLifecycleProject) Name() string { return p.name }
func (p *daemonLifecycleProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "daemon-lifecycle"}, nil
}
func (p *daemonLifecycleProject) Targets() []project.Target {
	return []project.Target{{Name: "dev", RootTasks: []string{"backend_debug", "frontend"}}}
}
func (p *daemonLifecycleProject) Tasks() []project.Task {
	service := func(name string) project.Task {
		return project.Task{
			Name:  name,
			Kind:  project.KindService,
			Ready: func(context.Context, *project.Runtime) error { return nil },
			Run: func(_ context.Context, rt *project.Runtime) error {
				handle := newDaemonLifecycleHandle()
				p.mu.Lock()
				p.handles[name] = append(p.handles[name], handle)
				p.mu.Unlock()
				rt.RegisterServiceHandle(handle)
				return nil
			},
		}
	}
	return []project.Task{service("backend_debug"), service("frontend")}
}
func (p *daemonLifecycleProject) snapshots(name string) []*daemonLifecycleHandle {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*daemonLifecycleHandle(nil), p.handles[name]...)
}

func TestDaemonRestartAndScopedStopPreserveIndependentService(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &daemonLifecycleProject{
		name:    "daemon-independent-lifecycle",
		handles: map[string][]*daemonLifecycleHandle{},
	}
	project.Register(p)
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		worktree:    worktree,
		instanceID:  inst.ID,
		projectName: p.name,
		logPath:     filepath.Join(worktree, ".devflow", "logs", inst.ID, "daemon.log"),
		subscribers: map[chan api.Event]bool{},
	}
	started, err := s.startActive(context.Background(), p.name, "dev", api.ModeWatch, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !started.Accepted || !started.DaemonStarted || started.Ready || started.State != "starting" {
		t.Fatalf("detached start did not distinguish acceptance from readiness: %+v", started)
	}
	defer s.stopActive(3 * time.Second)
	if !waitForDaemonCondition(3*time.Second, func() bool {
		return len(p.snapshots("backend_debug")) == 1 && len(p.snapshots("frontend")) == 1
	}) {
		t.Fatal("independent services did not start")
	}
	if !waitForDaemonCondition(3*time.Second, func() bool {
		again, startErr := s.startActive(context.Background(), p.name, "dev", api.ModeWatch, 2)
		return startErr == nil && again.Accepted && again.Ready && again.State == "ready"
	}) {
		t.Fatal("detached response did not report the already-ready target accurately")
	}
	frontend := p.snapshots("frontend")[0]
	preview, err := s.previewLifecycle(Request{Action: ActionRestart, Project: p.name, Task: "backend_debug", Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(preview.Plan.ServicesToRestart, ","); got != "backend_debug" {
		t.Fatalf("restart preview had wrong scope: %+v", preview.Plan)
	}
	if got := strings.Join(preview.Plan.ServicesToPreserve, ","); got != "frontend" {
		t.Fatalf("restart preview did not preserve independent frontend: %+v", preview.Plan)
	}
	if frontend.stopped.Load() || len(p.snapshots("backend_debug")) != 1 {
		t.Fatal("preview changed process state")
	}

	_, lifecycle, err := s.restart(context.Background(), p.name, "backend_debug", false, false, 2)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle == nil || !lifecycle.Success || len(lifecycle.Restarted) != 1 || lifecycle.Restarted[0] != "backend_debug" {
		t.Fatalf("restart did not report its exact affected set: %+v", lifecycle)
	}
	if got := strings.Join(lifecycle.Stopped, ","); got != "backend_debug" {
		t.Fatalf("running-service restart did not report its confirmed stop: %+v", lifecycle)
	}
	if len(lifecycle.Processes) != 1 || !lifecycle.Processes[0].Ready || lifecycle.Processes[0].Generation <= lifecycle.Processes[0].PreviousGeneration {
		t.Fatalf("restart did not verify its replacement generation/readiness: %+v", lifecycle.Processes)
	}
	if frontend.stopped.Load() || len(p.snapshots("frontend")) != 1 {
		t.Fatal("backend restart changed independent frontend")
	}

	stopResult, err := s.stopWork(context.Background(), false, "backend_debug")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stopResult.Stopped, ","); got != "backend_debug" {
		t.Fatalf("task-scoped stop returned %q", got)
	}
	if frontend.stopped.Load() {
		t.Fatal("task-scoped backend stop changed independent frontend")
	}
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active == nil {
		t.Fatal("task-scoped stop shut down the active run")
	}

	if _, err := s.stopWork(context.Background(), false, "missing"); err == nil || !strings.Contains(err.Error(), "unknown task") {
		t.Fatalf("unknown task stop returned %v", err)
	}
	if frontend.stopped.Load() {
		t.Fatal("unknown task stop changed independent frontend")
	}

	secondStop, err := s.stopWork(context.Background(), false, "backend_debug")
	if err != nil {
		t.Fatal(err)
	}
	if len(secondStop.Stopped) != 0 || secondStop.Lifecycle == nil || !secondStop.Lifecycle.Success {
		t.Fatalf("already-stopped task was not idempotent: %+v", secondStop)
	}

	stoppedPreview, err := s.previewLifecycle(Request{Action: ActionRestart, Project: p.name, Task: "backend_debug", Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(stoppedPreview.Plan.ProcessesToStop) != 0 {
		t.Fatalf("stopped-service restart preview invented a process to stop: %+v", stoppedPreview.Plan)
	}
	_, restartedStopped, err := s.restart(context.Background(), p.name, "backend_debug", false, false, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(restartedStopped.Stopped) != 0 || strings.Join(restartedStopped.Restarted, ",") != "backend_debug" {
		t.Fatalf("stopped-service restart reported a phantom stop: %+v", restartedStopped)
	}
	if len(restartedStopped.Processes) != 1 || restartedStopped.Processes[0].PreviousPID != 0 || restartedStopped.Processes[0].PreviousGeneration != 0 || !restartedStopped.Processes[0].Ready {
		t.Fatalf("stopped-service restart did not preserve an absent previous identity: %+v", restartedStopped.Processes)
	}
}

func TestStopAllPlanAndActualIncludeEveryActiveService(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &daemonLifecycleProject{
		name:    "daemon-stop-all-lifecycle",
		handles: map[string][]*daemonLifecycleHandle{},
	}
	project.Register(p)
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		worktree:    worktree,
		instanceID:  inst.ID,
		projectName: p.name,
		logPath:     filepath.Join(worktree, ".devflow", "logs", inst.ID, "daemon.log"),
		subscribers: map[chan api.Event]bool{},
	}
	if _, err := s.startActive(context.Background(), p.name, "dev", api.ModeWatch, 2); err != nil {
		t.Fatal(err)
	}
	if !waitForDaemonCondition(3*time.Second, func() bool {
		return len(p.snapshots("backend_debug")) == 1 && len(p.snapshots("frontend")) == 1
	}) {
		t.Fatal("independent services did not start")
	}
	preview, err := s.previewLifecycle(Request{Action: ActionStop, All: true, Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(preview.Plan.ProcessesToStop, ","); got != "backend_debug,daemon,frontend" {
		t.Fatalf("stop-all preview scope = %q", got)
	}
	result, err := s.stopWork(context.Background(), true, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(result.Stopped, ","); got != "backend_debug,daemon,frontend" {
		t.Fatalf("stop-all actual scope = %q, result=%+v", got, result)
	}
	if got := strings.Join(result.Lifecycle.Plan.ProcessesToStop, ","); got != strings.Join(result.Stopped, ",") {
		t.Fatalf("successful stop-all plan/result differ: plan=%q actual=%q", got, strings.Join(result.Stopped, ","))
	}
	if !p.snapshots("backend_debug")[0].stopped.Load() || !p.snapshots("frontend")[0].stopped.Load() {
		t.Fatal("stop-all result completed before both services were physically stopped")
	}
}

func TestAddUnconfirmedLifecycleIssuesPreservesPartialActualResult(t *testing.T) {
	result := &api.LifecycleResult{
		Plan:    api.LifecyclePlan{ProcessesToStop: []string{"backend", "daemon", "frontend"}},
		Stopped: []string{"backend"},
	}
	addUnconfirmedLifecycleIssues(result, errors.New("synthetic termination failure"))
	if len(result.Issues) != 2 || result.Issues[0].Resource != "daemon" || result.Issues[1].Resource != "frontend" {
		t.Fatalf("partial stop differences were not explicit and deterministic: %+v", result.Issues)
	}
	for _, issue := range result.Issues {
		if !strings.Contains(issue.Reason, "synthetic termination failure") {
			t.Fatalf("partial stop issue omitted its reason: %+v", issue)
		}
	}
}

func waitForDaemonCondition(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return condition()
}

func TestDetachedTargetStateDistinguishesStartingReadyAndFailed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{worktree: worktree, instanceID: inst.ID}
	if ready, state := s.detachedTargetState("dev"); ready || state != "starting" {
		t.Fatalf("missing status = ready=%v state=%q", ready, state)
	}
	if err := instance.SaveStatus(worktree, inst.ID, "dev", api.ModeWatch, map[string]api.NodeStatus{
		"build":   {Name: "build", State: api.StateDone},
		"backend": {Name: "backend", State: api.StateRunning, Ready: true},
	}); err != nil {
		t.Fatal(err)
	}
	if ready, state := s.detachedTargetState("dev"); !ready || state != "ready" {
		t.Fatalf("healthy status = ready=%v state=%q", ready, state)
	}
	if err := instance.SaveStatus(worktree, inst.ID, "dev", api.ModeWatch, map[string]api.NodeStatus{
		"backend": {Name: "backend", State: api.StateFailed, LastError: "exit status 17"},
	}); err != nil {
		t.Fatal(err)
	}
	if ready, state := s.detachedTargetState("dev"); ready || state != "failed" {
		t.Fatalf("failed status = ready=%v state=%q", ready, state)
	}
	if ready, state := s.detachedTargetState("other"); ready || state != "starting" {
		t.Fatalf("stale other-target status = ready=%v state=%q", ready, state)
	}
}

func TestInvalidateAndRelaunchForcesMatchingActiveRunToRestart(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("HOME", cacheRoot)
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("LOCALAPPDATA", cacheRoot)
	name := "daemon-invalidate-force-restart"
	project.Register(daemonTestProject{
		name: name,
		tasks: []project.Task{
			{
				Name:    "build",
				Kind:    project.KindOnce,
				Cache:   true,
				Outputs: project.Outputs{Files: []string{"build.out"}},
				Run: func(_ context.Context, rt *project.Runtime) error {
					return os.WriteFile(filepath.Join(rt.Worktree, "build.out"), []byte("built"), 0o644)
				},
			},
		},
		targets: []project.Target{{Name: "main", RootTasks: []string{"build"}}},
	})
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	inst.LastRun = api.RunConfig{
		Project:     name,
		Target:      "main",
		Mode:        api.ModeDev,
		MaxParallel: 1,
		Detached:    true,
	}
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}
	if err := instance.SaveStatus(worktree, inst.ID, "main", api.ModeDev, map[string]api.NodeStatus{
		"build": {Name: "build", Kind: "once", State: api.StateCached, LastRunKey: "cached"},
	}); err != nil {
		t.Fatal(err)
	}
	oldCtx, cancelOld := context.WithCancel(context.Background())
	oldDone := make(chan struct{})
	go func() {
		<-oldCtx.Done()
		close(oldDone)
	}()
	s := &Server{
		worktree:    worktree,
		instanceID:  inst.ID,
		projectName: name,
		logPath:     filepath.Join(worktree, ".devflow", "logs", inst.ID, "daemon.log"),
		active: &activeRun{
			projectName: name,
			target:      "main",
			mode:        api.ModeDev,
			maxParallel: 1,
			cancel:      cancelOld,
			done:        oldDone,
		},
	}
	defer s.stopActive(3 * time.Second)

	if _, err := s.invalidateAndRelaunch(context.Background(), "build"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-oldDone:
	case <-time.After(time.Second):
		t.Fatal("expected invalidate relaunch to stop the existing matching active run")
	}
	s.mu.Lock()
	relaunched := s.active
	s.mu.Unlock()
	if relaunched != nil {
		select {
		case <-relaunched.done:
		case <-time.After(3 * time.Second):
			t.Fatal("relaunched build did not finish")
		}
	}
	state, err := instance.LoadStatus(worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Nodes["build"].State != api.StateDirty && state.Nodes["build"].State != api.StateDone && state.Nodes["build"].State != api.StateCached {
		t.Fatalf("expected build to be invalidated or already rerun, got %+v", state.Nodes["build"])
	}
}

type daemonTestProject struct {
	name    string
	tasks   []project.Task
	targets []project.Target
}

func (p daemonTestProject) Name() string { return p.name }
func (p daemonTestProject) Tasks() []project.Task {
	return append([]project.Task(nil), p.tasks...)
}
func (p daemonTestProject) Targets() []project.Target {
	return append([]project.Target(nil), p.targets...)
}
func (p daemonTestProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: p.name}, nil
}

func copyFileForDaemonTest(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
