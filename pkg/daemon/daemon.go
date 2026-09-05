package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/benjaco/devflow/internal/execution"
	"github.com/benjaco/devflow/internal/executionconflict"
	"github.com/benjaco/devflow/internal/executionstate"
	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/internal/lock"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/cache"
	"github.com/benjaco/devflow/pkg/database"
	"github.com/benjaco/devflow/pkg/engine"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

type Action string

const (
	ActionPing        Action = "ping"
	ActionRun         Action = "run"
	ActionWatch       Action = "watch"
	ActionFlush       Action = "flush"
	ActionStop        Action = "stop"
	ActionStatus      Action = "status"
	ActionSubscribe   Action = "subscribe"
	ActionRestart     Action = "restart"
	ActionInvalidate  Action = "invalidate"
	ActionRetarget    Action = "retarget"
	ActionListActions Action = "action_list"
	ActionRunAction   Action = "action_run"
)

type Request struct {
	ID           string            `json:"id,omitempty"`
	Action       Action            `json:"action"`
	Project      string            `json:"project,omitempty"`
	Target       string            `json:"target,omitempty"`
	Mode         api.RunMode       `json:"mode,omitempty"`
	MaxParallel  int               `json:"maxParallel,omitempty"`
	Detach       bool              `json:"detach,omitempty"`
	StreamEvents bool              `json:"streamEvents,omitempty"`
	TimeoutMs    int64             `json:"timeoutMs,omitempty"`
	Task         string            `json:"task,omitempty"`
	All          bool              `json:"all,omitempty"`
	Upstream     bool              `json:"upstream,omitempty"`
	Downstream   bool              `json:"downstream,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	ActionID     string            `json:"actionId,omitempty"`
	ActionKind   string            `json:"actionKind,omitempty"`
	Component    string            `json:"component,omitempty"`
	Inputs       map[string]string `json:"inputs,omitempty"`
	Preview      bool              `json:"preview,omitempty"`
}

type Response struct {
	ID               string                `json:"id,omitempty"`
	OK               bool                  `json:"ok"`
	Error            string                `json:"error,omitempty"`
	Code             string                `json:"code,omitempty"`
	ResourceConflict *api.ResourceConflict `json:"resourceConflict,omitempty"`
	Started          *StartResult          `json:"started,omitempty"`
	Run              *api.RunResult        `json:"run,omitempty"`
	Flush            *api.FlushResult      `json:"flush,omitempty"`
	Stop             *StopResult           `json:"stop,omitempty"`
	Status           *api.StatusResult     `json:"status,omitempty"`
	Actions          *ActionListResult     `json:"actions,omitempty"`
	ActionResult     *ActionRunResult      `json:"actionResult,omitempty"`
	Lifecycle        *api.LifecycleResult  `json:"lifecycle,omitempty"`
}

type StartResult struct {
	InstanceID    string      `json:"instanceId"`
	Target        string      `json:"target"`
	Mode          api.RunMode `json:"mode"`
	DaemonPID     int         `json:"daemonPid"`
	LogPath       string      `json:"logPath"`
	Accepted      bool        `json:"accepted"`
	DaemonStarted bool        `json:"daemonStarted"`
	Ready         bool        `json:"ready"`
	State         string      `json:"state"`
}

type StopResult struct {
	InstanceID string               `json:"instanceId"`
	Stopped    []string             `json:"stopped"`
	Lifecycle  *api.LifecycleResult `json:"lifecycle,omitempty"`
}

type ActionListResult struct {
	Project string           `json:"project"`
	Actions []project.Action `json:"actions"`
}

type ActionRunResult struct {
	RunID        string            `json:"runId,omitempty"`
	ActionID     string            `json:"actionId"`
	Kind         string            `json:"kind,omitempty"`
	Component    string            `json:"component,omitempty"`
	Status       string            `json:"status"`
	Inputs       map[string]string `json:"inputs,omitempty"`
	CreatedFiles []string          `json:"createdFiles,omitempty"`
	Run          *api.RunResult    `json:"run,omitempty"`
	Relaunch     *StartResult      `json:"relaunch,omitempty"`
}

type frame struct {
	Type     string     `json:"type"`
	ID       string     `json:"id,omitempty"`
	Event    *api.Event `json:"event,omitempty"`
	Response *Response  `json:"response,omitempty"`
}

const (
	responseFrameType    = "response"
	responseAckFrameType = "response_ack"
	responseAckTimeout   = time.Second
)

type Options struct {
	Worktree string
	Project  string
	LogPath  string
}

type Server struct {
	worktree    string
	instanceID  string
	projectName string
	logPath     string
	socketPath  string

	// transitionMu covers admission and persisted mutation, never foreground execution.
	transitionMu sync.Mutex
	generation   uint64
	closing      bool
	mu           sync.Mutex
	active       *activeRun
	subscribers  map[chan api.Event]bool
	eventMu      sync.Mutex
	shutdown     chan struct{}
	shutdownMu   sync.Once
}

type activeRun struct {
	projectName string
	target      string
	mode        api.RunMode
	maxParallel int
	cancel      context.CancelFunc
	done        chan struct{}
	result      *api.RunResult
	err         error
	startedAt   time.Time
	controller  *engine.LifecycleController
	generation  uint64
	lease       *execution.Lease
	stopping    bool
}

type Client struct {
	worktree   string
	instanceID string
	socketPath string
}

var (
	startDaemonForEnsure         = startDaemonProcess
	skipDaemonBinaryCheckForTest bool
)

func SetStartDaemonFuncForTest(fn func(worktree, instanceID, projectName string) error) func() {
	previous := startDaemonForEnsure
	previousSkip := skipDaemonBinaryCheckForTest
	if fn == nil {
		startDaemonForEnsure = startDaemonProcess
		skipDaemonBinaryCheckForTest = false
	} else {
		startDaemonForEnsure = fn
		skipDaemonBinaryCheckForTest = true
	}
	return func() {
		startDaemonForEnsure = previous
		skipDaemonBinaryCheckForTest = previousSkip
	}
}

func Serve(ctx context.Context, opts Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	root, id, err := resolveWorktreeAndID(opts.Worktree)
	if err != nil {
		return err
	}
	socketPath, err := instance.DaemonSocketPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return err
	}
	logPath := opts.LogPath
	if logPath == "" {
		logPath = filepath.Join(root, ".devflow", "logs", id, "daemon.log")
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	inst := &api.Instance{ID: id, Worktree: root}
	if err := instance.RecordDaemon(inst, os.Getpid(), logPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socketPath)

	s := &Server{
		worktree:    root,
		instanceID:  id,
		projectName: strings.TrimSpace(opts.Project),
		logPath:     logPath,
		socketPath:  socketPath,
		subscribers: map[chan api.Event]bool{},
		shutdown:    make(chan struct{}),
	}
	s.writeDaemonLog("daemon started pid=%d worktree=%s project=%s socket=%s", os.Getpid(), root, s.projectName, socketPath)

	go func() {
		select {
		case <-ctx.Done():
		case <-s.shutdown:
		}
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.stopActive(5 * time.Second)
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				s.stopActive(5 * time.Second)
				return nil
			}
			continue
		}
		go s.handleConn(ctx, conn)
	}
}

func Ensure(ctx context.Context, worktree, projectName string) (*Client, bool, error) {
	root, id, err := resolveWorktreeAndID(worktree)
	if err != nil {
		return nil, false, err
	}
	socketPath, err := instance.DaemonSocketPath(id)
	if err != nil {
		return nil, false, err
	}
	client := &Client{worktree: root, instanceID: id, socketPath: socketPath}
	if err := client.Ping(ctx); err == nil && !daemonNeedsRestart(root, id) {
		_ = ensureDaemonLog(root, id)
		return client, false, nil
	}
	startLock, err := lock.Acquire(daemonLockPath(root, id))
	if err != nil {
		return nil, false, err
	}
	defer startLock.Release()
	if err := client.Ping(ctx); err == nil {
		if !daemonNeedsRestart(root, id) {
			_ = ensureDaemonLog(root, id)
			return client, false, nil
		}
		if err := stopExistingDaemon(ctx, root, id, client); err != nil {
			return nil, false, err
		}
	}
	if err := startDaemonForEnsure(root, id, projectName); err != nil {
		return nil, false, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := client.Ping(ctx); err == nil {
			return client, true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, false, fmt.Errorf("timed out waiting for devflow daemon for %s", root)
}

func stopExistingDaemon(ctx context.Context, worktree, instanceID string, client *Client) error {
	callCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if _, err := client.Call(callCtx, Request{Action: ActionStop, All: true}); err != nil {
		return fmt.Errorf("cannot replace active daemon: %w", err)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		pingCtx, pingCancel := context.WithTimeout(ctx, 200*time.Millisecond)
		err := client.Ping(pingCtx)
		pingCancel()
		if err != nil && ctx.Err() == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for previous daemon to stop; ownership retained")
		case <-ticker.C:
		}
	}
}

func Dial(worktree string) (*Client, error) {
	root, id, err := resolveWorktreeAndID(worktree)
	if err != nil {
		return nil, err
	}
	socketPath, err := instance.DaemonSocketPath(id)
	if err != nil {
		return nil, err
	}
	return &Client{worktree: root, instanceID: id, socketPath: socketPath}, nil
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Call(ctx, Request{Action: ActionPing})
	return err
}

func (c *Client) Call(ctx context.Context, req Request, onEvent ...func(api.Event)) (Response, error) {
	if req.ID == "" {
		req.ID = requestID()
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if err := enc.Encode(req); err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, err
	}
	for {
		var fr frame
		if err := dec.Decode(&fr); err != nil {
			if ctx.Err() != nil {
				return Response{}, ctx.Err()
			}
			return Response{}, err
		}
		if fr.Type == "event" && fr.Event != nil {
			for _, fn := range onEvent {
				if fn != nil {
					fn(*fr.Event)
				}
			}
			continue
		}
		if fr.Type != responseFrameType || fr.Response == nil {
			continue
		}
		resp := *fr.Response
		// A Windows Unix-domain socket can discard a just-written response when
		// the server closes immediately afterward. Acknowledge the terminal
		// frame after decoding it so the daemon can close gracefully. A failed
		// ACK cannot undo the received result or make a completed mutation fail.
		_ = enc.Encode(frame{Type: responseAckFrameType, ID: req.ID})
		if !resp.OK {
			if resp.Error == "" {
				resp.Error = "daemon request failed"
			}
			if resp.Code == "resource_conflict" {
				conflict := &execution.ConflictError{Cause: errors.New(resp.Error)}
				if detail := resp.ResourceConflict; detail != nil {
					conflict.Worktree = detail.Worktree
					conflict.Owner = &execution.Owner{Worktree: detail.Worktree, PID: detail.PID, Target: detail.Target, Mode: detail.Mode, Kind: detail.Kind}
					conflict.RecoveryRequired = detail.RecoveryRequired
				}
				return resp, conflict
			}
			return resp, fmt.Errorf("%s", resp.Error)
		}
		return resp, nil
	}
}

func (c *Client) Subscribe(ctx context.Context, onEvent func(api.Event)) error {
	req := Request{ID: requestID(), Action: ActionSubscribe}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	dec := json.NewDecoder(conn)
	for {
		var fr frame
		if err := dec.Decode(&fr); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if fr.Type == "event" && fr.Event != nil && onEvent != nil {
			onEvent(*fr.Event)
		}
	}
}

func startDaemonProcess(worktree, instanceID, projectName string) error {
	inst := &api.Instance{ID: instanceID, Worktree: worktree}
	executable, err := daemonExecutable(worktree)
	if err != nil {
		return err
	}
	logPath := filepath.Join(worktree, ".devflow", "logs", instanceID, "daemon.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := logFile.Chmod(0o600); err != nil {
		_ = logFile.Close()
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(executable, "__internal_daemon", "--worktree", worktree, "--project", projectName, "--log-path", logPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Dir = worktree
	prepareDaemonCmd(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return instance.RecordDaemon(inst, cmd.Process.Pid, logPath)
}

func daemonExecutable(worktree string) (string, error) {
	current, err := os.Executable()
	if err != nil {
		return "", err
	}
	target := daemonExecutablePath(worktree)
	if err := fsutil.CopyFile(current, target); err != nil {
		return "", err
	}
	if err := os.Chmod(target, 0o755); err != nil {
		return "", err
	}
	return target, nil
}

func daemonExecutablePath(worktree string) string {
	name := "devflow-daemon"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(worktree, ".devflow", "bin", name)
}

func daemonNeedsRestart(worktree, instanceID string) bool {
	if skipDaemonBinaryCheckForTest {
		return false
	}
	_ = instanceID
	current, err := os.Executable()
	if err != nil {
		return false
	}
	target := daemonExecutablePath(worktree)
	return !sameFileContents(current, target)
}

func sameFileContents(left, right string) bool {
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		return false
	}
	if leftInfo.Size() != rightInfo.Size() {
		return false
	}
	leftData, err := os.ReadFile(left)
	if err != nil {
		return false
	}
	rightData, err := os.ReadFile(right)
	if err != nil {
		return false
	}
	return bytes.Equal(leftData, rightData)
}

func ensureDaemonLog(worktree, instanceID string) error {
	daemonRef, err := instance.LoadDaemon(worktree, instanceID)
	if err != nil {
		return err
	}
	logPath := daemonRef.LogPath
	if logPath == "" {
		logPath = filepath.Join(worktree, ".devflow", "logs", instanceID, "daemon.log")
		if err := instance.RecordDaemon(&api.Instance{ID: instanceID, Worktree: worktree}, daemonRef.PID, logPath); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func resolveWorktreeAndID(worktree string) (string, string, error) {
	root := strings.TrimSpace(worktree)
	var err error
	if root == "" {
		root, err = os.Getwd()
	} else {
		root, err = filepath.Abs(root)
	}
	if err != nil {
		return "", "", err
	}
	id, real, err := instance.IDForWorktree(root)
	if err != nil {
		return "", "", err
	}
	return real, id, nil
}

func requestID() string {
	return fmt.Sprintf("daemon-%d-%d", time.Now().UTC().UnixNano(), os.Getpid())
}

func daemonLockPath(worktree, instanceID string) string {
	return filepath.Join(worktree, ".devflow", "state", "instances", instanceID, "daemon.lock")
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	var writeMu sync.Mutex
	writeFrame := func(fr frame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return enc.Encode(fr)
	}
	var req Request
	if err := dec.Decode(&req); err != nil {
		return
	}
	if req.ID == "" {
		req.ID = requestID()
	}
	if req.Action == ActionSubscribe {
		ch := s.addSubscriber()
		defer s.removeSubscriber(ch)
		// Subscriptions have no further client frames. Observe the read side
		// so disconnecting an idle client releases its subscription immediately,
		// even if no later event would expose a failed write.
		disconnected := make(chan struct{})
		go func() {
			var unexpected frame
			_ = dec.Decode(&unexpected)
			close(disconnected)
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case <-disconnected:
				return
			case evt := <-ch:
				if err := writeFrame(frame{Type: "event", ID: req.ID, Event: &evt}); err != nil {
					return
				}
			}
		}
	}
	var eventCh chan api.Event
	if req.StreamEvents {
		eventCh = s.addSubscriber()
		defer s.removeSubscriber(eventCh)
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-done:
					return
				case evt := <-eventCh:
					if err := writeFrame(frame{Type: "event", ID: req.ID, Event: &evt}); err != nil {
						return
					}
				}
			}
		}()
	}
	resp := s.handleRequest(ctx, req)
	if err := writeFrame(frame{Type: responseFrameType, ID: req.ID, Response: &resp}); err != nil {
		return
	}
	awaitResponseAck(conn, dec, req.ID)
	// A stop-all preview is read-only. Only the executed action may tear down
	// the daemon after its response has been delivered.
	if req.Action == ActionStop && req.All && !req.Preview && resp.OK {
		s.requestShutdown()
	}
}

func awaitResponseAck(conn net.Conn, dec *json.Decoder, requestID string) {
	if conn == nil || dec == nil {
		return
	}
	if err := conn.SetReadDeadline(time.Now().Add(responseAckTimeout)); err != nil {
		return
	}
	var ack frame
	if err := dec.Decode(&ack); err != nil {
		return
	}
	if ack.Type != responseAckFrameType || ack.ID != requestID {
		return
	}
}

func (s *Server) handleRequest(ctx context.Context, req Request) Response {
	resp := Response{ID: req.ID, OK: true}
	switch req.Action {
	case ActionPing:
		return resp
	case ActionStatus:
		status, err := s.statusResult()
		if err != nil {
			return errorResponse(req.ID, err)
		}
		resp.Status = status
		return resp
	case ActionRun:
		if req.Detach {
			started, err := s.startActive(ctx, req.Project, req.Target, normalizeMode(req.Mode, api.ModeDev), req.MaxParallel)
			if err != nil {
				return errorResponse(req.ID, err)
			}
			resp.Started = started
			return resp
		}
		result, err := s.runAttached(ctx, req.Project, req.Target, normalizeMode(req.Mode, api.ModeDev), req.MaxParallel, nil)
		if result != nil {
			resp.Run = result
		}
		if err != nil {
			return responseWithError(resp, err)
		}
		return resp
	case ActionWatch:
		started, err := s.startActive(ctx, req.Project, req.Target, api.ModeWatch, req.MaxParallel)
		if err != nil {
			return errorResponse(req.ID, err)
		}
		resp.Started = started
		return resp
	case ActionFlush:
		timeout := time.Duration(req.TimeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		result, err := s.flush(ctx, req.Project, req.Target, timeout, req.MaxParallel)
		resp.Flush = &result
		if err != nil {
			return responseWithError(resp, err)
		}
		return resp
	case ActionStop:
		if req.Preview {
			lifecycle, err := s.previewLifecycle(req)
			resp.Lifecycle = lifecycle
			if err != nil {
				return responseWithError(resp, err)
			}
			return resp
		}
		result, err := s.stopWork(ctx, req.All, req.Task)
		if result != nil {
			resp.Stop = result
			resp.Lifecycle = result.Lifecycle
		}
		if err != nil {
			return responseWithError(resp, err)
		}
		return resp
	case ActionRestart:
		if req.Preview {
			lifecycle, err := s.previewLifecycle(req)
			resp.Lifecycle = lifecycle
			if err != nil {
				return responseWithError(resp, err)
			}
			return resp
		}
		result, lifecycle, err := s.restart(ctx, req.Project, req.Task, req.Upstream, req.Downstream, req.MaxParallel)
		if result != nil {
			resp.Run = result
		}
		resp.Lifecycle = lifecycle
		if err != nil {
			return responseWithError(resp, err)
		}
		return resp
	case ActionInvalidate:
		if req.Preview {
			lifecycle, err := s.previewLifecycle(req)
			resp.Lifecycle = lifecycle
			if err != nil {
				return responseWithError(resp, err)
			}
			return resp
		}
		lifecycle, err := s.invalidateAndRelaunch(ctx, req.Task)
		resp.Lifecycle = lifecycle
		if err != nil {
			return responseWithError(resp, err)
		}
		return resp
	case ActionRetarget:
		if req.Preview {
			lifecycle, err := s.previewLifecycle(req)
			resp.Lifecycle = lifecycle
			if err != nil {
				return responseWithError(resp, err)
			}
			return resp
		}
		lifecycle, err := s.retarget(ctx, req.Target)
		resp.Lifecycle = lifecycle
		if err != nil {
			return responseWithError(resp, err)
		}
		return resp
	case ActionListActions:
		result, err := s.listActions(req.Project)
		if err != nil {
			return errorResponse(req.ID, err)
		}
		resp.Actions = result
		return resp
	case ActionRunAction:
		result, err := s.runProjectAction(ctx, req.Project, req.ActionID, req.ActionKind, req.Component, req.Inputs, req.Env)
		if result != nil {
			resp.ActionResult = result
			resp.Run = result.Run
			resp.Started = result.Relaunch
		}
		if err != nil {
			return responseWithError(resp, err)
		}
		return resp
	default:
		return errorResponse(req.ID, fmt.Errorf("unknown daemon action %q", req.Action))
	}
}

func errorResponse(id string, err error) Response {
	return responseWithError(Response{ID: id}, err)
}

func responseWithError(resp Response, err error) Response {
	resp.OK = false
	resp.Error = err.Error()
	if detail := executionconflict.Details(err); detail != nil {
		resp.Code = "resource_conflict"
		resp.ResourceConflict = detail
	}
	return resp
}

func normalizeMode(got, fallback api.RunMode) api.RunMode {
	if got == "" {
		return fallback
	}
	return got
}

func (s *Server) addSubscriber() chan api.Event {
	ch := make(chan api.Event, 128)
	s.mu.Lock()
	s.subscribers[ch] = true
	s.mu.Unlock()
	return ch
}

func (s *Server) removeSubscriber(ch chan api.Event) {
	s.mu.Lock()
	delete(s.subscribers, ch)
	close(ch)
	s.mu.Unlock()
}

func (s *Server) publish(evt api.Event) {
	s.persistEvent(evt)
	s.mu.Lock()
	for ch := range s.subscribers {
		select {
		case ch <- evt:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *Server) publishStatus(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	s.publish(api.Event{
		TS:         process.NowRFC3339Nano(),
		Type:       api.EventLogLine,
		InstanceID: s.instanceID,
		Worktree:   s.worktree,
		Task:       "daemon",
		Stream:     "status",
		Line:       line,
	})
}

func (s *Server) persistEvent(evt api.Event) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	path := instance.EventsPath(s.worktree, s.instanceID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_ = file.Chmod(0o600)
	_ = json.NewEncoder(file).Encode(evt)
}

func (s *Server) writeDaemonLog(format string, args ...any) {
	if s.logPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.logPath), 0o755); err != nil {
		return
	}
	file, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_ = file.Chmod(0o600)
	_, _ = fmt.Fprintf(file, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func (s *Server) requestShutdown() {
	s.shutdownMu.Do(func() {
		s.writeDaemonLog("daemon shutdown requested")
		close(s.shutdown)
	})
}

func (s *Server) resolveProject(projectName string) (string, project.Project, error) {
	name := strings.TrimSpace(projectName)
	if name == "" {
		name = strings.TrimSpace(s.projectName)
	}
	if name != "" {
		p, err := project.Lookup(name)
		if err != nil {
			return "", nil, err
		}
		return p.Name(), p, nil
	}
	names := project.Names()
	if len(names) == 1 {
		p, err := project.Lookup(names[0])
		if err != nil {
			return "", nil, err
		}
		return p.Name(), p, nil
	}
	p, err := project.Detect(s.worktree)
	if err == nil {
		return p.Name(), p, nil
	}
	if len(names) == 0 {
		return "", nil, fmt.Errorf("no project is registered")
	}
	return "", nil, fmt.Errorf("multiple projects are registered; pass --project explicitly")
}

func (s *Server) resolveTarget(p project.Project, target string) (project.Project, string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		target = project.PreferredTarget(p)
	}
	if target == "" {
		return nil, "", fmt.Errorf("no target was provided and project %q does not define a preferred target", p.Name())
	}
	return project.ResolveExecutionProject(p, target)
}

func (s *Server) startActive(ctx context.Context, projectName, target string, mode api.RunMode, maxParallel int) (*StartResult, error) {
	s.transitionMu.Lock()
	defer s.transitionMu.Unlock()
	return s.startActiveLocked(ctx, projectName, target, mode, maxParallel)
}

func (s *Server) startActiveLocked(ctx context.Context, projectName, target string, mode api.RunMode, maxParallel int) (*StartResult, error) {
	if s.closing {
		return nil, errors.New("daemon is shutting down")
	}
	projectName, p, err := s.resolveProject(projectName)
	if err != nil {
		return nil, err
	}
	execProject, resolvedTarget, err := s.resolveTarget(p, target)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	active := s.active
	matches := active != nil && !active.stopping && active.projectName == projectName && active.target == resolvedTarget && active.mode == mode
	s.mu.Unlock()
	if !matches {
		if _, err := s.beginRunLocked(ctx, projectName, execProject, resolvedTarget, mode, maxParallel, true, nil); err != nil {
			return nil, err
		}
	}
	result := &StartResult{InstanceID: s.instanceID, Target: resolvedTarget, Mode: mode, DaemonPID: os.Getpid(), LogPath: s.logPath, Accepted: true, DaemonStarted: true}
	result.Ready, result.State = s.detachedTargetState(resolvedTarget)
	return result, nil
}

// beginRunLocked reserves the full mutation lifetime, including environment restoration.
// Callers release transitionMu before waiting on done so stop and status remain available.
func (s *Server) beginRunLocked(ctx context.Context, projectName string, p project.Project, target string, mode api.RunMode, maxParallel int, detached bool, env map[string]string) (*activeRun, error) {
	if s.closing {
		return nil, errors.New("daemon is shutting down")
	}
	if !s.stopActiveLocked(5 * time.Second) {
		return nil, s.activeStopConflict()
	}
	lease := execution.FromContext(ctx)
	var err error
	if lease == nil || !lease.ValidFor(s.worktree) {
		lease, err = executionstate.Acquire(s.worktree, execution.Owner{Kind: "daemon", Target: target, Mode: string(mode)})
	}
	if err != nil {
		return nil, err
	}
	if err := s.recordRun(projectName, target, mode, maxParallel, detached); err != nil {
		_ = lease.Release()
		return nil, err
	}
	if detached {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(execution.ContextWithLease(ctx, lease))
	s.generation++
	active := &activeRun{projectName: projectName, target: target, mode: mode, maxParallel: maxParallel, cancel: cancel, done: make(chan struct{}), startedAt: time.Now().UTC(), controller: engine.NewLifecycleController(), generation: s.generation, lease: lease}
	s.mu.Lock()
	s.active = active
	s.mu.Unlock()
	go func() {
		defer s.finishActiveRun(active)
		restore, err := applyTemporaryInstanceEnv(s.worktree, s.instanceID, env)
		if err != nil {
			active.err = err
			return
		}
		s.runEngine(runCtx, p, target, mode, maxParallel, active)
		if err := restore(); err != nil {
			active.err = errors.Join(active.err, err)
			lease.RequireRecovery()
		}
	}()
	return active, nil
}

func (s *Server) finishActiveRun(active *activeRun) {
	active.cancel()
	if active.lease != nil {
		active.err = errors.Join(active.err, active.lease.Release())
	}
	s.mu.Lock()
	if s.active == active {
		s.active = nil
	}
	close(active.done)
	s.mu.Unlock()
}

// detachedTargetState is a response-time snapshot, not a promise that the
// target will remain in this state. Detached callers use it to distinguish an
// accepted launch from a target that is already ready or has already failed.
func (s *Server) detachedTargetState(target string) (bool, string) {
	status, err := instance.LoadStatus(s.worktree, s.instanceID)
	// recordRun updates the selected target before the engine publishes its
	// first status snapshot. Never attribute a prior target's terminal state to
	// the newly accepted launch during that small interval.
	if err != nil || status.Target != target || len(status.Nodes) == 0 {
		return false, "starting"
	}
	ready := true
	for _, node := range status.Nodes {
		switch node.State {
		case api.StateFailed, api.StateBlocked, api.StateCanceled:
			return false, "failed"
		case api.StateDegraded:
			return false, "degraded"
		case api.StateDone, api.StateCached, api.StateReady:
			// Terminal one-time tasks and ready service nodes satisfy the snapshot.
		case api.StateRunning:
			if !node.Ready {
				ready = false
			}
		default:
			ready = false
		}
	}
	if ready {
		return true, "ready"
	}
	return false, "starting"
}

func (s *Server) runAttached(ctx context.Context, projectName, target string, mode api.RunMode, maxParallel int, env map[string]string) (*api.RunResult, error) {
	projectName, p, err := s.resolveProject(projectName)
	if err != nil {
		return nil, err
	}
	return s.runAttachedProject(ctx, projectName, p, target, mode, maxParallel, env)
}

func (s *Server) runAttachedProject(ctx context.Context, projectName string, p project.Project, target string, mode api.RunMode, maxParallel int, env map[string]string) (*api.RunResult, error) {
	s.transitionMu.Lock()
	active, err := s.startAttachedProjectLocked(ctx, projectName, p, target, mode, maxParallel, env)
	s.transitionMu.Unlock()
	if err != nil {
		return nil, err
	}
	<-active.done
	return active.result, active.err
}

func (s *Server) startAttachedProjectLocked(ctx context.Context, projectName string, p project.Project, target string, mode api.RunMode, maxParallel int, env map[string]string) (*activeRun, error) {
	execProject, resolvedTarget, err := s.resolveTarget(p, target)
	if err != nil {
		return nil, err
	}
	return s.beginRunLocked(ctx, projectName, execProject, resolvedTarget, mode, maxParallel, false, env)
}

func (s *Server) runEngine(ctx context.Context, p project.Project, target string, mode api.RunMode, maxParallel int, active *activeRun) {
	eng, err := engine.New(p, s.worktree)
	if err != nil {
		active.err = err
		return
	}
	events := eng.SubscribeEvents()
	stopEvents := make(chan struct{})
	doneEvents := make(chan struct{})
	go func() {
		defer close(doneEvents)
		for {
			select {
			case evt := <-events:
				s.publish(evt)
			case <-stopEvents:
				for {
					select {
					case evt := <-events:
						s.publish(evt)
					default:
						return
					}
				}
			}
		}
	}()
	req := engine.Request{
		Target:              target,
		Worktree:            s.worktree,
		Mode:                mode,
		MaxParallel:         maxParallel,
		LifecycleController: active.controller,
	}
	switch mode {
	case api.ModeWatch:
		active.err = eng.Watch(ctx, req)
	default:
		outcome, runErr := eng.Run(ctx, req)
		if outcome != nil {
			active.result = &outcome.Result
		}
		active.err = runErr
	}
	close(stopEvents)
	<-doneEvents
}

func (s *Server) stopActive(timeout time.Duration) bool {
	s.transitionMu.Lock()
	defer s.transitionMu.Unlock()
	return s.stopActiveLocked(timeout)
}

func (s *Server) stopActiveLocked(timeout time.Duration) bool {
	s.mu.Lock()
	active := s.active
	if active != nil {
		active.stopping = true
	}
	s.mu.Unlock()
	if active == nil {
		return true
	}
	if active.cancel != nil {
		active.cancel()
	}
	if timeout <= 0 {
		<-active.done
		return true
	}
	select {
	case <-active.done:
		s.mu.Lock()
		if s.active == active {
			s.active = nil
		}
		s.mu.Unlock()
		return true
	case <-time.After(timeout):
		return false
	}
}

func (s *Server) activeStopConflict() error {
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	owner := execution.Owner{Worktree: s.worktree, PID: os.Getpid(), Kind: "daemon"}
	if active != nil {
		owner.Target = active.target
		owner.Mode = string(active.mode)
	}
	return &execution.ConflictError{Owner: &owner, Cause: errors.New("timed out waiting for active execution to stop; ownership retained")}
}

// acquireStopLease permits explicit stop-all recovery only when all remaining
// resources can be identified and confirmed stopped while the lock is held.
func (s *Server) acquireStopLease(ctx context.Context, all bool) (*execution.Lease, []string, error) {
	var recovered []string
	var options []execution.Option
	if all {
		options = append(options, execution.WithRecovery(func(previous execution.Owner) error {
			if previous.PID != os.Getpid() && instance.ProcessAlive(previous.PID) {
				return errors.New("previous execution process is still alive")
			}
			inst, err := instance.Load(s.worktree, s.instanceID)
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			if os.IsNotExist(err) {
				return errors.New("previous execution has no recoverable resource inventory")
			}
			if err := s.validateStopInventory(true); err != nil {
				return err
			}
			refs := additionalRecordedProcessRefs(s.worktree, s.instanceID, inst)
			names, err := instance.StopDaemonWork(inst, refs, os.Getpid())
			recovered = append(recovered, names...)
			if err != nil {
				return err
			}
			if inst.DB.ContainerName != "" {
				stopped, err := database.New().StopRuntimeIfRunning(ctx, inst.DB)
				if err != nil {
					return err
				}
				if stopped {
					recovered = append(recovered, "database")
				}
			}
			if err := markAllStoppedNodes(s.worktree, s.instanceID); err != nil {
				return err
			}
			return nil
		}))
	}
	lease, err := execution.Acquire(s.worktree, execution.Owner{Kind: "daemon-stop"}, options...)
	if err == nil && all {
		if inventoryErr := s.validateStopInventory(false); inventoryErr != nil {
			owner := lease.Owner()
			return nil, recovered, errors.Join(&execution.ConflictError{Owner: &owner, RecoveryRequired: true, Cause: inventoryErr}, lease.Release())
		}
	}
	return lease, recovered, err
}

func (s *Server) validateStopInventory(required bool) error {
	status, err := instance.LoadStatus(s.worktree, s.instanceID)
	if os.IsNotExist(err) {
		if required {
			return errors.New("previous execution has no recoverable task inventory")
		}
		return nil
	}
	if err != nil {
		return err
	}
	names := make([]string, 0, len(status.Nodes))
	for name := range status.Nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		node := status.Nodes[name]
		if node.PID > 0 {
			continue
		}
		if required && node.Generation > 0 && node.State != api.StateStopped {
			return fmt.Errorf("cannot confirm cleanup of PID-less resource %q; reconcile its external resource before recovery", name)
		}
		switch node.State {
		case api.StateRunning, api.StateStarting, api.StateReady, api.StateRestarting, api.StateDegraded:
			return fmt.Errorf("cannot confirm subprocess cleanup for interrupted task %q", name)
		}
	}
	return nil
}

func (s *Server) recordRun(projectName, target string, mode api.RunMode, maxParallel int, detached bool) error {
	inst, err := instance.Resolve(s.worktree, filepath.Base(s.worktree))
	if err != nil {
		return err
	}
	inst.LastRun = api.RunConfig{
		Project:     projectName,
		Target:      target,
		Mode:        mode,
		MaxParallel: maxParallel,
		Detached:    detached,
	}
	return instance.Save(inst)
}

func (s *Server) flush(ctx context.Context, projectName, target string, timeout time.Duration, maxParallel int) (api.FlushResult, error) {
	s.transitionMu.Lock()
	unlock := sync.OnceFunc(s.transitionMu.Unlock)
	defer unlock()
	startedAt := time.Now().UTC()
	requestID := fmt.Sprintf("flush-%d-%d", startedAt.UnixNano(), os.Getpid())
	inst, instErr := instance.Load(s.worktree, s.instanceID)
	if strings.TrimSpace(projectName) == "" && instErr == nil && inst.LastRun.Project != "" {
		projectName = inst.LastRun.Project
	}
	projectName, p, err := s.resolveProject(projectName)
	if err != nil {
		result := newFlushResult(requestID, s.worktree, s.instanceID, projectName, target, startedAt)
		result.Issues = append(result.Issues, api.FlushIssue{Kind: "project_error", Message: err.Error()})
		return result, fmt.Errorf("flush failed")
	}
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if strings.TrimSpace(target) == "" {
		if active != nil && active.mode == api.ModeWatch {
			target = active.target
		} else if instErr == nil && inst.LastRun.Target != "" {
			target = inst.LastRun.Target
		} else {
			target = project.PreferredTarget(p)
		}
	}
	_, resolvedTarget, err := s.resolveTarget(p, target)
	if err != nil {
		result := newFlushResult(requestID, s.worktree, s.instanceID, projectName, target, startedAt)
		result.Issues = append(result.Issues, api.FlushIssue{Kind: "target_error", Message: err.Error()})
		return result, fmt.Errorf("flush failed")
	}
	startedWatch := false
	if active != nil {
		if active.mode != api.ModeWatch {
			result := newFlushResult(requestID, s.worktree, s.instanceID, projectName, resolvedTarget, startedAt)
			result.Issues = append(result.Issues, api.FlushIssue{
				Kind:    "non_watch_execution",
				Message: fmt.Sprintf("live daemon work is running mode %q, want %q", active.mode, api.ModeWatch),
				LogPath: s.logPath,
			})
			return result, fmt.Errorf("flush failed")
		}
		if active.target != resolvedTarget {
			result := newFlushResult(requestID, s.worktree, s.instanceID, projectName, resolvedTarget, startedAt)
			result.Issues = append(result.Issues, api.FlushIssue{
				Kind:    "target_mismatch",
				Message: fmt.Sprintf("live watch target is %q, requested %q", active.target, resolvedTarget),
				LogPath: s.logPath,
			})
			return result, fmt.Errorf("flush failed")
		}
	} else {
		// LastRun is a restart preference; only s.active proves work is running.
		watchStartedAt := time.Now().UTC()
		if _, err := s.startActiveLocked(ctx, projectName, resolvedTarget, api.ModeWatch, maxParallel); err != nil {
			return s.flushFailure(requestID, projectName, resolvedTarget, startedAt, false, "watch_start_error", err)
		}
		startedWatch = true
		unlock()
		if !waitForWatchReady(s.worktree, s.instanceID, watchStartedAt, time.Until(startedAt.Add(timeout))) {
			result := newFlushResult(requestID, s.worktree, s.instanceID, projectName, resolvedTarget, startedAt)
			result.Started = true
			result.TimedOut = true
			result.Issues = append(result.Issues, api.FlushIssue{Kind: "timeout", Message: "timed out waiting for detached watch daemon to become ready"})
			return result, fmt.Errorf("flush failed")
		}
	}
	unlock()
	req := api.FlushRequest{
		ID:        requestID,
		CreatedAt: startedAt,
		SyncPath:  instance.FlushSyncPath(s.worktree, s.instanceID, requestID),
	}
	if err := instance.WriteFlushRequest(s.worktree, s.instanceID, req); err != nil {
		return s.flushFailure(requestID, projectName, resolvedTarget, startedAt, startedWatch, "request_write_error", err)
	}
	if err := os.MkdirAll(filepath.Dir(req.SyncPath), 0o755); err != nil {
		return s.flushFailure(requestID, projectName, resolvedTarget, startedAt, startedWatch, "sync_prepare_error", err)
	}
	if err := os.WriteFile(req.SyncPath, []byte(requestID+"\n"), 0o644); err != nil {
		return s.flushFailure(requestID, projectName, resolvedTarget, startedAt, startedWatch, "sync_write_error", err)
	}
	result, ok, err := waitForFlushAck(s.worktree, s.instanceID, requestID, req.SyncPath, time.Until(startedAt.Add(timeout)))
	if err != nil {
		return s.flushFailure(requestID, projectName, resolvedTarget, startedAt, startedWatch, "ack_read_error", err)
	}
	if !ok {
		result = newFlushResult(requestID, s.worktree, s.instanceID, projectName, resolvedTarget, startedAt)
		result.Started = startedWatch
		result.TimedOut = true
		result.Issues = append(result.Issues, api.FlushIssue{Kind: "timeout", Message: "timed out waiting for flush acknowledgement"})
		return result, fmt.Errorf("flush failed")
	}
	result.Started = startedWatch
	if result.Project == "" {
		result.Project = projectName
	}
	if !result.Success {
		return result, fmt.Errorf("flush failed")
	}
	return result, nil
}

func (s *Server) stopWork(ctx context.Context, all bool, task string) (*StopResult, error) {
	s.transitionMu.Lock()
	defer s.transitionMu.Unlock()
	return s.stopWorkLocked(ctx, all, task)
}

func (s *Server) stopWorkLocked(ctx context.Context, all bool, task string) (_ *StopResult, resultErr error) {
	s.generation++
	var mutationLease *execution.Lease
	var stopped []string
	mutationStarted := false
	defer func() {
		if mutationLease == nil {
			return
		}
		if resultErr != nil && mutationStarted {
			resultErr = errors.Join(resultErr, mutationLease.Abandon())
		} else {
			resultErr = errors.Join(resultErr, mutationLease.Release())
		}
	}()
	inst, err := instance.Load(s.worktree, s.instanceID)
	if os.IsNotExist(err) {
		mutationLease, stopped, err = s.acquireStopLease(ctx, all)
		if err != nil {
			return nil, err
		}
		inst, err = instance.Resolve(s.worktree, filepath.Base(s.worktree))
	}
	if err != nil {
		return nil, err
	}
	var lifecycle *api.LifecycleResult
	if all {
		snapshot := s.captureStopAll(inst)
		plan := snapshot.plan
		lifecycle = &api.LifecycleResult{
			Plan:      plan,
			Affected:  []string{},
			Stopped:   []string{},
			Restarted: []string{},
			Processes: []api.LifecycleProcessChange{},
			Success:   false,
		}
		activeStopped := s.stopActiveLocked(5 * time.Second)
		for name, candidate := range snapshot.processes {
			if !candidate.active {
				continue
			}
			if candidate.pid > 0 {
				if !instance.ProcessAlive(candidate.pid) {
					stopped = append(stopped, name)
				}
			} else if activeStopped {
				stopped = append(stopped, name)
			}
		}
		if !activeStopped {
			err = s.activeStopConflict()
			lifecycle.Error = err.Error()
			lifecycle.Stopped = uniqueSortedStrings(stopped)
			lifecycle.Affected = append([]string(nil), lifecycle.Stopped...)
			addUnconfirmedLifecycleIssues(lifecycle, err)
			return &StopResult{InstanceID: s.instanceID, Stopped: lifecycle.Stopped, Lifecycle: lifecycle}, err
		}
		if mutationLease == nil {
			var recovered []string
			mutationLease, recovered, err = s.acquireStopLease(ctx, true)
			stopped = append(stopped, recovered...)
			if err != nil {
				lifecycle.Error = err.Error()
				return &StopResult{InstanceID: s.instanceID, Lifecycle: lifecycle}, err
			}
		}
		mutationStarted = true
		var loadErr error
		inst, loadErr = instance.Load(s.worktree, s.instanceID)
		if loadErr != nil {
			lifecycle.Error = loadErr.Error()
			lifecycle.Stopped = uniqueSortedStrings(stopped)
			lifecycle.Affected = append([]string(nil), lifecycle.Stopped...)
			addUnconfirmedLifecycleIssues(lifecycle, loadErr)
			return &StopResult{InstanceID: s.instanceID, Stopped: lifecycle.Stopped, Lifecycle: lifecycle}, loadErr
		}
		remainingStopped, processErr := instance.StopDaemonWork(inst, snapshot.extra, os.Getpid())
		stopped = append(stopped, remainingStopped...)
		if err == nil {
			err = processErr
		} else if processErr != nil {
			err = errors.Join(err, processErr)
		}
	} else {
		if strings.TrimSpace(task) == "" {
			return nil, fmt.Errorf("usage: devflow stop --task <name> | --all")
		}
		plan, planErr := s.planTaskStop(inst, task)
		if planErr != nil {
			return nil, planErr
		}
		lifecycle = &api.LifecycleResult{
			Plan:      plan,
			Affected:  []string{},
			Stopped:   []string{},
			Restarted: []string{},
			Processes: []api.LifecycleProcessChange{},
			Success:   true,
		}
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if active == nil || active.controller == nil {
			if mutationLease == nil {
				mutationLease, _, err = s.acquireStopLease(ctx, false)
				if err != nil {
					lifecycle.Error = err.Error()
					return &StopResult{InstanceID: s.instanceID, Lifecycle: lifecycle}, err
				}
			}
			mutationStarted = true
		}
		for _, name := range plan.ProcessesToStop {
			if active != nil && active.controller != nil {
				change, stopErr := active.controller.Stop(ctx, name)
				if stopErr != nil {
					lifecycle.Success = false
					lifecycle.Error = stopErr.Error()
					return &StopResult{InstanceID: s.instanceID, Stopped: lifecycle.Stopped, Lifecycle: lifecycle}, stopErr
				}
				if change.Stopped {
					lifecycle.Stopped = append(lifecycle.Stopped, name)
					lifecycle.Affected = append(lifecycle.Affected, name)
				} else {
					lifecycle.Issues = append(lifecycle.Issues, api.LifecycleIssue{Resource: name, Reason: "process was already absent"})
				}
				lifecycle.Processes = append(lifecycle.Processes, api.LifecycleProcessChange{
					Task:               name,
					PreviousPID:        change.Previous.PID,
					PreviousGeneration: change.Previous.Generation,
				})
				continue
			}
			var names []string
			names, err = instance.StopProcesses(inst, name)
			stopped = append(stopped, names...)
			if err != nil {
				lifecycle.Success = false
				lifecycle.Error = err.Error()
				return &StopResult{InstanceID: s.instanceID, Stopped: uniqueSortedStrings(stopped), Lifecycle: lifecycle}, err
			}
			lifecycle.Stopped = append(lifecycle.Stopped, names...)
			lifecycle.Affected = append(lifecycle.Affected, names...)
		}
		lifecycle.Stopped = uniqueSortedStrings(lifecycle.Stopped)
		lifecycle.Affected = uniqueSortedStrings(lifecycle.Affected)
		_ = markStoppedNodes(s.worktree, s.instanceID, lifecycle.Stopped)
		return &StopResult{InstanceID: s.instanceID, Stopped: lifecycle.Stopped, Lifecycle: lifecycle}, nil
	}
	if err != nil {
		lifecycle.Error = err.Error()
		lifecycle.Stopped = uniqueSortedStrings(stopped)
		lifecycle.Affected = append([]string(nil), lifecycle.Stopped...)
		addUnconfirmedLifecycleIssues(lifecycle, err)
		return &StopResult{InstanceID: s.instanceID, Stopped: lifecycle.Stopped, Lifecycle: lifecycle}, err
	}
	if all && inst.DB.ContainerName != "" {
		databaseStopped, stopErr := database.New().StopRuntimeIfRunning(ctx, inst.DB)
		if stopErr != nil {
			lifecycle.Error = stopErr.Error()
			lifecycle.Stopped = uniqueSortedStrings(stopped)
			lifecycle.Affected = append([]string(nil), lifecycle.Stopped...)
			addUnconfirmedLifecycleIssues(lifecycle, stopErr)
			return &StopResult{InstanceID: s.instanceID, Stopped: lifecycle.Stopped, Lifecycle: lifecycle}, stopErr
		}
		if databaseStopped {
			stopped = append(stopped, "database")
		} else {
			// The persisted container name made database part of the preview, but
			// the engine found no live runtime to terminate. Keep that plan/result
			// difference explicit instead of reporting a phantom stop.
			lifecycle.Issues = append(lifecycle.Issues, api.LifecycleIssue{
				Resource: "database",
				Reason:   "container was already absent or stopped",
			})
		}
	}
	if all {
		s.closing = true
		stopped = append(stopped, "daemon")
		inst.Processes = map[string]api.ProcessRef{}
		if err := instance.ClearDaemon(inst); err != nil {
			return nil, err
		}
		if err := instance.Save(inst); err != nil {
			return nil, err
		}
		stopped = uniqueSortedStrings(stopped)
		lifecycle.Stopped = append([]string(nil), stopped...)
		lifecycle.Affected = append([]string(nil), stopped...)
		lifecycle.Success = true
	}
	if all {
		_ = markAllStoppedNodes(s.worktree, s.instanceID)
	} else {
		_ = markStoppedNodes(s.worktree, s.instanceID, stopped)
	}
	return &StopResult{InstanceID: s.instanceID, Stopped: stopped, Lifecycle: lifecycle}, nil
}

type stopAllCandidate struct {
	pid    int
	active bool
}

type stopAllSnapshot struct {
	plan      api.LifecyclePlan
	processes map[string]stopAllCandidate
	extra     map[string]int
}

// captureStopAll freezes the intended stop scope before cancellation clears
// the engine-owned service registry and persisted process references.
func (s *Server) captureStopAll(inst *api.Instance) stopAllSnapshot {
	extra := additionalRecordedProcessRefs(s.worktree, s.instanceID, inst)
	status, _ := instance.LoadStatus(s.worktree, s.instanceID)
	s.mu.Lock()
	hasActiveRun := s.active != nil
	s.mu.Unlock()

	processes := map[string]stopAllCandidate{}
	pidOwners := map[int]string{}
	addPID := func(name string, pid int, active bool) {
		if name == "" || pid <= 0 || !instance.ProcessAlive(pid) {
			return
		}
		if owner, exists := pidOwners[pid]; exists {
			candidate := processes[owner]
			candidate.active = candidate.active || active
			processes[owner] = candidate
			return
		}
		pidOwners[pid] = name
		processes[name] = stopAllCandidate{pid: pid, active: active}
	}

	processNames := make([]string, 0, len(inst.Processes))
	for name := range inst.Processes {
		processNames = append(processNames, name)
	}
	sort.Strings(processNames)
	for _, name := range processNames {
		ref := inst.Processes[name]
		active := false
		if status != nil {
			active = lifecycleNodeActive(status.Nodes[name].State)
		}
		addPID(name, ref.PID, active)
	}
	if status != nil {
		names := make([]string, 0, len(status.Nodes))
		for name := range status.Nodes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			node := status.Nodes[name]
			if !lifecycleNodeActive(node.State) {
				continue
			}
			if node.PID > 0 {
				addPID(name, node.PID, true)
			} else if hasActiveRun {
				processes[name] = stopAllCandidate{active: true}
			}
		}
	}
	extraNames := make([]string, 0, len(extra))
	for name := range extra {
		extraNames = append(extraNames, name)
	}
	sort.Strings(extraNames)
	for _, name := range extraNames {
		addPID(name, extra[name], false)
	}

	toStop := map[string]bool{"daemon": true}
	for name := range processes {
		toStop[name] = true
	}
	if inst.DB.ContainerName != "" {
		toStop["database"] = true
	}
	return stopAllSnapshot{
		plan: api.LifecyclePlan{
			RequestedAction:         "stop-all",
			SelectedTarget:          inst.LastRun.Target,
			TasksToInvalidate:       []string{},
			ProcessesToStop:         sortedStringSet(toStop),
			TasksToExecute:          []string{},
			ServicesToPreserve:      []string{},
			ServicesToRestart:       []string{},
			ConfirmationRecommended: true,
		},
		processes: processes,
		extra:     extra,
	}
}

func addUnconfirmedLifecycleIssues(result *api.LifecycleResult, stopErr error) {
	if result == nil || stopErr == nil {
		return
	}
	stopped := make(map[string]bool, len(result.Stopped))
	for _, name := range result.Stopped {
		stopped[name] = true
	}
	for _, name := range result.Plan.ProcessesToStop {
		if stopped[name] {
			continue
		}
		result.Issues = append(result.Issues, api.LifecycleIssue{Resource: name, Reason: stopErr.Error()})
	}
}

func (s *Server) planTaskStop(inst *api.Instance, task string) (api.LifecyclePlan, error) {
	task = strings.TrimSpace(task)
	plan := api.LifecyclePlan{
		RequestedAction:         "stop",
		SelectedTask:            task,
		TasksToInvalidate:       []string{},
		ProcessesToStop:         []string{},
		TasksToExecute:          []string{},
		ServicesToPreserve:      []string{},
		ServicesToRestart:       []string{},
		ConfirmationRecommended: false,
	}
	if task == "" {
		return plan, fmt.Errorf("usage: devflow stop --task <name> | --all")
	}
	toStop := map[string]bool{}
	known := false
	if _, ok := inst.Processes[task]; ok {
		known = true
	}
	state, _ := instance.LoadStatus(s.worktree, s.instanceID)
	if state != nil {
		if node, ok := state.Nodes[task]; ok {
			known = true
			if lifecycleNodeActive(node.State) {
				toStop[task] = true
			}
		}
	}

	if ref, ok := inst.Processes[task]; ok && ref.PID > 0 {
		toStop[task] = true
	}
	if name := strings.TrimSpace(inst.LastRun.Project); name != "" && inst.LastRun.Target != "" {
		if p, lookupErr := project.Lookup(name); lookupErr == nil {
			if g, target, graphErr := executionGraphForProject(p, inst.LastRun.Target); graphErr == nil {
				if _, ok := g.Tasks[task]; ok {
					known = true
					closure, _ := g.TargetClosure(target)
					inClosure := make(map[string]bool, len(closure))
					for _, name := range closure {
						inClosure[name] = true
					}
					for _, name := range g.Downstream([]string{task}) {
						def, ok := g.Tasks[name]
						if !ok || !inClosure[name] || !project.IsServiceKind(def.Kind) {
							continue
						}
						ref, running := inst.Processes[name]
						node, tracked := api.NodeStatus{}, false
						if state != nil {
							node, tracked = state.Nodes[name]
						}
						if (running && ref.PID > 0) || (tracked && lifecycleNodeActive(node.State)) {
							toStop[name] = true
						}
					}
				}
			}
		}
	}
	if !known {
		return plan, fmt.Errorf("unknown task %q", task)
	}
	plan.ProcessesToStop = sortedStringSet(toStop)
	for name, ref := range inst.Processes {
		if ref.PID > 0 && !toStop[name] {
			plan.ServicesToPreserve = append(plan.ServicesToPreserve, name)
		}
	}
	if state != nil {
		for name, node := range state.Nodes {
			if lifecycleNodeActive(node.State) && !toStop[name] && !containsString(plan.ServicesToPreserve, name) {
				plan.ServicesToPreserve = append(plan.ServicesToPreserve, name)
			}
		}
	}
	sort.Strings(plan.ServicesToPreserve)
	plan.ConfirmationRecommended = len(plan.ProcessesToStop) > 1
	return plan, nil
}

func (s *Server) previewLifecycle(req Request) (*api.LifecycleResult, error) {
	inst, err := instance.Load(s.worktree, s.instanceID)
	if err != nil {
		return nil, err
	}
	status, _ := instance.LoadStatus(s.worktree, s.instanceID)
	result := &api.LifecycleResult{
		Affected:  []string{},
		Stopped:   []string{},
		Restarted: []string{},
		Processes: []api.LifecycleProcessChange{},
		Success:   true,
	}
	switch req.Action {
	case ActionStop:
		if req.All {
			result.Plan = s.captureStopAll(inst).plan
			return result, nil
		}
		result.Plan, err = s.planTaskStop(inst, req.Task)
		return result, err
	case ActionRestart:
		projectName := req.Project
		if strings.TrimSpace(projectName) == "" {
			projectName = inst.LastRun.Project
		}
		_, p, resolveErr := s.resolveProject(projectName)
		if resolveErr != nil {
			return nil, resolveErr
		}
		g, graphErr := graph.New(p.Tasks(), p.Targets())
		if graphErr != nil {
			return nil, graphErr
		}
		selected, closureErr := restartClosure(g, req.Task, req.Upstream, req.Downstream)
		if closureErr != nil {
			return nil, closureErr
		}
		result.Plan = restartLifecyclePlan(g, inst, status, req.Task, selected)
		return result, nil
	case ActionInvalidate:
		projectName, p, resolveErr := s.resolveProject(inst.LastRun.Project)
		_ = projectName
		if resolveErr != nil {
			return nil, resolveErr
		}
		g, target, graphErr := executionGraphForProject(p, inst.LastRun.Target)
		if graphErr != nil {
			return nil, graphErr
		}
		def, ok := g.Tasks[req.Task]
		if !ok {
			return nil, fmt.Errorf("unknown task %q", req.Task)
		}
		if project.IsServiceKind(def.Kind) {
			selected, closureErr := restartClosure(g, req.Task, false, false)
			if closureErr != nil {
				return nil, closureErr
			}
			result.Plan = restartLifecyclePlan(g, inst, status, req.Task, selected)
			result.Plan.RequestedAction = "rerun"
			return result, nil
		}
		invalidated, invalidErr := downstreamInvalidateTasks(g, target, req.Task)
		if invalidErr != nil {
			return nil, invalidErr
		}
		closure, closureErr := g.TargetClosure(target)
		if closureErr != nil {
			return nil, closureErr
		}
		result.Plan = api.LifecyclePlan{
			RequestedAction:         "rerun",
			SelectedTask:            req.Task,
			SelectedTarget:          target,
			TasksToInvalidate:       invalidated,
			ProcessesToStop:         sortedProcessNames(inst.Processes),
			TasksToExecute:          closure,
			ServicesToPreserve:      []string{},
			ServicesToRestart:       serviceNamesInTasks(g, closure),
			ConfirmationRecommended: true,
		}
		return result, nil
	case ActionRetarget:
		_, p, resolveErr := s.resolveProject(inst.LastRun.Project)
		if resolveErr != nil {
			return nil, resolveErr
		}
		execProject, target, targetErr := project.ResolveExecutionProject(p, req.Target)
		if targetErr != nil {
			return nil, targetErr
		}
		g, graphErr := graph.New(execProject.Tasks(), execProject.Targets())
		if graphErr != nil {
			return nil, graphErr
		}
		closure, closureErr := g.TargetClosure(target)
		if closureErr != nil {
			return nil, closureErr
		}
		result.Plan = api.LifecyclePlan{
			RequestedAction:         "retarget",
			SelectedTarget:          target,
			TasksToInvalidate:       []string{},
			ProcessesToStop:         sortedProcessNames(inst.Processes),
			TasksToExecute:          closure,
			ServicesToPreserve:      []string{},
			ServicesToRestart:       serviceNamesInTasks(g, closure),
			ConfirmationRecommended: true,
		}
		return result, nil
	default:
		return nil, fmt.Errorf("action %q does not support lifecycle planning", req.Action)
	}
}

func lifecycleNodeActive(state api.NodeState) bool {
	switch state {
	case api.StateStarting, api.StateReady, api.StateRunning, api.StateRestarting, api.StateDegraded:
		return true
	default:
		return false
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sortedStringSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func uniqueSortedStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (s *Server) statusResult() (*api.StatusResult, error) {
	inst, err := instance.Load(s.worktree, s.instanceID)
	if err != nil {
		return nil, err
	}
	state, err := instance.LoadStatus(s.worktree, s.instanceID)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		state = &instance.State{Target: inst.LastRun.Target, Mode: inst.LastRun.Mode, Nodes: map[string]api.NodeStatus{}}
	}
	nodes := make([]api.NodeStatus, 0, len(state.Nodes))
	for _, node := range state.Nodes {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return &api.StatusResult{
		InstanceID: s.instanceID,
		Worktree:   s.worktree,
		Target:     state.Target,
		Mode:       state.Mode,
		UpdatedAt:  state.UpdatedAt,
		Ports:      inst.Ports,
		DB:         instance.DisplayDB(inst.DB),
		URLs:       InstanceURLs(inst),
		Daemon:     DaemonStatus(inst),
		Nodes:      nodes,
	}, nil
}

func newFlushResult(requestID, worktree, instanceID, projectName, target string, startedAt time.Time) api.FlushResult {
	now := time.Now().UTC()
	return api.FlushResult{
		RequestID:  requestID,
		InstanceID: instanceID,
		Worktree:   worktree,
		Project:    projectName,
		Target:     target,
		Mode:       api.ModeWatch,
		Success:    false,
		DurationMs: now.Sub(startedAt).Milliseconds(),
		UpdatedAt:  now,
	}
}

func (s *Server) flushFailure(requestID, projectName, target string, startedAt time.Time, started bool, kind string, cause error) (api.FlushResult, error) {
	// FlushResult is the automation-facing diagnostic surface. Returning only the
	// Go error here would make the daemon protocol serialize an all-zero result.
	result := newFlushResult(requestID, s.worktree, s.instanceID, projectName, target, startedAt)
	result.Started = started
	if executionconflict.Details(cause) != nil {
		kind = "resource_conflict"
	}
	result.Issues = append(result.Issues, api.FlushIssue{
		Kind:    kind,
		Message: cause.Error(),
		LogPath: s.logPath,
	})
	return result, fmt.Errorf("flush failed: %w", cause)
}

func waitForFlushAck(worktree, instanceID, requestID, syncPath string, timeout time.Duration) (api.FlushResult, bool, error) {
	if timeout <= 0 {
		return api.FlushResult{}, false, nil
	}
	deadline := time.Now().Add(timeout)
	retouchInterval := 100 * time.Millisecond
	nextTouch := time.Now().Add(retouchInterval)
	for time.Now().Before(deadline) {
		result, err := instance.LoadFlushAck(worktree, instanceID, requestID)
		if err == nil {
			return result, true, nil
		}
		if !os.IsNotExist(err) {
			return api.FlushResult{}, false, err
		}
		if syncPath != "" && !time.Now().Before(nextTouch) {
			_ = os.WriteFile(syncPath, []byte(requestID+"\n"+time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o644)
			nextTouch = time.Now().Add(retouchInterval)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return api.FlushResult{}, false, nil
}

func waitForWatchReady(worktree, instanceID string, after time.Time, timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	deadline := time.Now().Add(timeout)
	path := instance.FlushWatchReadyPath(worktree, instanceID)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			readyAt, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
			if parseErr == nil && !readyAt.Before(after) {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func (s *Server) invalidateAndRelaunch(ctx context.Context, task string) (*api.LifecycleResult, error) {
	s.transitionMu.Lock()
	unlock := sync.OnceFunc(s.transitionMu.Unlock)
	defer unlock()
	return s.invalidateAndRelaunchLocked(ctx, task, unlock)
}

func (s *Server) invalidateAndRelaunchLocked(ctx context.Context, task string, unlock func()) (*api.LifecycleResult, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return nil, fmt.Errorf("no task selected")
	}
	inst, err := instance.Load(s.worktree, s.instanceID)
	if err != nil {
		return nil, err
	}
	projectName, p, err := s.resolveProject(inst.LastRun.Project)
	if err != nil {
		return nil, err
	}
	if inst.LastRun.Target == "" {
		return nil, fmt.Errorf("instance has no recorded project/target to relaunch")
	}
	g, resolvedTarget, err := executionGraphForProject(p, inst.LastRun.Target)
	if err != nil {
		return nil, err
	}
	selectedDef, ok := g.Tasks[task]
	if !ok {
		return nil, fmt.Errorf("unknown task %q", task)
	}
	if project.IsServiceKind(selectedDef.Kind) {
		_, lifecycle, restartErr := s.restartLocked(ctx, projectName, task, false, false, inst.LastRun.MaxParallel, unlock)
		if lifecycle != nil {
			lifecycle.Plan.RequestedAction = "rerun"
		}
		return lifecycle, restartErr
	}
	toInvalidate, err := downstreamInvalidateTasks(g, resolvedTarget, task)
	if err != nil {
		return nil, err
	}
	closure, err := g.TargetClosure(resolvedTarget)
	if err != nil {
		return nil, err
	}
	plan := api.LifecyclePlan{
		RequestedAction:         "rerun",
		SelectedTask:            task,
		SelectedTarget:          resolvedTarget,
		TasksToInvalidate:       append([]string(nil), toInvalidate...),
		ProcessesToStop:         sortedProcessNames(inst.Processes),
		TasksToExecute:          append([]string(nil), closure...),
		ServicesToPreserve:      []string{},
		ServicesToRestart:       serviceNamesInTasks(g, closure),
		ConfirmationRecommended: true,
	}
	lifecycle := &api.LifecycleResult{
		Plan:      plan,
		Affected:  []string{},
		Stopped:   []string{},
		Restarted: []string{},
		Processes: []api.LifecycleProcessChange{},
		Success:   false,
	}
	if !s.stopActiveLocked(5 * time.Second) {
		err := s.activeStopConflict()
		lifecycle.Error = err.Error()
		return lifecycle, err
	}
	lease, err := executionstate.Acquire(s.worktree, execution.Owner{Kind: "daemon", Target: resolvedTarget, Mode: string(inst.LastRun.Mode)})
	if err != nil {
		lifecycle.Error = err.Error()
		return lifecycle, err
	}
	transferred := false
	defer func() {
		if !transferred {
			_ = lease.Release()
		}
	}()
	ctx = execution.ContextWithLease(ctx, lease)
	if err := writeInvalidateTransition(s.worktree, s.instanceID, resolvedTarget, g, toInvalidate); err != nil {
		lifecycle.Error = err.Error()
		return lifecycle, err
	}
	s.publishStatus("invalidated downstream from %s, relaunching %s", task, inst.LastRun.Target)
	store := cache.NewNamespaced(instance.CacheRoot(), project.CacheNamespace(p))
	for _, name := range toInvalidate {
		if err := store.Invalidate(name); err != nil {
			lifecycle.Error = err.Error()
			return lifecycle, err
		}
		if err := instance.RemoveTaskStamp(s.worktree, s.instanceID, name); err != nil {
			lifecycle.Error = err.Error()
			return lifecycle, err
		}
	}
	_, err = s.startActiveLocked(ctx, projectName, inst.LastRun.Target, inst.LastRun.Mode, inst.LastRun.MaxParallel)
	transferred = err == nil
	lifecycle.Affected = append(lifecycle.Affected, toInvalidate...)
	lifecycle.Stopped = append(lifecycle.Stopped, plan.ProcessesToStop...)
	lifecycle.Restarted = append(lifecycle.Restarted, plan.ServicesToRestart...)
	lifecycle.Success = err == nil
	if err != nil {
		lifecycle.Error = err.Error()
	}
	return lifecycle, err
}

func (s *Server) restart(ctx context.Context, projectName, task string, upstream, downstream bool, maxParallel int) (*api.RunResult, *api.LifecycleResult, error) {
	s.transitionMu.Lock()
	unlock := sync.OnceFunc(s.transitionMu.Unlock)
	defer unlock()
	return s.restartLocked(ctx, projectName, task, upstream, downstream, maxParallel, unlock)
}

func (s *Server) restartLocked(ctx context.Context, projectName, task string, upstream, downstream bool, maxParallel int, unlock func()) (*api.RunResult, *api.LifecycleResult, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return nil, nil, fmt.Errorf("usage: devflow restart <task>")
	}
	projectName, p, err := s.resolveProject(projectName)
	if err != nil {
		return nil, nil, err
	}
	g, err := graph.New(p.Tasks(), p.Targets())
	if err != nil {
		return nil, nil, err
	}
	selected, err := restartClosure(g, task, upstream, downstream)
	if err != nil {
		return nil, nil, err
	}
	taskDef, ok := g.Tasks[task]
	if !ok {
		return nil, nil, fmt.Errorf("unknown task %q", task)
	}
	inst, err := instance.Load(s.worktree, s.instanceID)
	if err != nil {
		return nil, nil, err
	}
	status, _ := instance.LoadStatus(s.worktree, s.instanceID)
	plan := restartLifecyclePlan(g, inst, status, task, selected)
	lifecycle := &api.LifecycleResult{
		Plan:      plan,
		Affected:  []string{},
		Stopped:   []string{},
		Restarted: []string{},
		Processes: []api.LifecycleProcessChange{},
		Success:   false,
	}
	if project.IsServiceKind(taskDef.Kind) {
		if inst.LastRun.Target == "" {
			err := fmt.Errorf("service restart requires a previously detached run for this instance")
			lifecycle.Error = err.Error()
			return nil, lifecycle, err
		}
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if active == nil || active.controller == nil {
			if len(plan.ServicesToPreserve) > 0 {
				restartErr := fmt.Errorf("cannot restart service %q without its owning active run while preserving %s; no restart occurred", task, strings.Join(plan.ServicesToPreserve, ", "))
				lifecycle.Error = restartErr.Error()
				return nil, lifecycle, restartErr
			}
			// A prior startup failure has no live engine left to own a
			// service-local replacement. Relaunch the recorded detached target;
			// unlike the old same-target path this always starts real work.
			started, startErr := s.startActiveLocked(ctx, projectName, inst.LastRun.Target, inst.LastRun.Mode, inst.LastRun.MaxParallel)
			if startErr != nil {
				lifecycle.Error = startErr.Error()
				return nil, lifecycle, startErr
			}
			unlock()
			readyTimeout := taskDef.ReadyTimeout
			if readyTimeout <= 0 {
				readyTimeout = 10 * time.Second
			}
			readyNode, readyErr := waitForTaskTerminalReadiness(ctx, s.worktree, s.instanceID, task, readyTimeout)
			if readyErr != nil {
				lifecycle.Error = readyErr.Error()
				return nil, lifecycle, readyErr
			}
			lifecycle.Affected = append(lifecycle.Affected, task)
			lifecycle.Restarted = append(lifecycle.Restarted, task)
			lifecycle.Processes = append(lifecycle.Processes, api.LifecycleProcessChange{
				Task:       task,
				PID:        readyNode.PID,
				Generation: readyNode.Generation,
				Ready:      true,
			})
			lifecycle.Success = started.Accepted
			return nil, lifecycle, nil
		}
		unlock()
		for _, name := range plan.ServicesToRestart {
			change, restartErr := active.controller.Restart(ctx, name)
			if restartErr != nil {
				lifecycle.Error = restartErr.Error()
				return nil, lifecycle, restartErr
			}
			if change.Previous.Generation == change.Current.Generation || !change.Ready {
				restartErr = fmt.Errorf("service %q did not produce a ready replacement", name)
				lifecycle.Error = restartErr.Error()
				return nil, lifecycle, restartErr
			}
			lifecycle.Affected = append(lifecycle.Affected, name)
			if change.Stopped {
				lifecycle.Stopped = append(lifecycle.Stopped, name)
			}
			lifecycle.Restarted = append(lifecycle.Restarted, name)
			lifecycle.Processes = append(lifecycle.Processes, api.LifecycleProcessChange{
				Task:               name,
				PreviousPID:        change.Previous.PID,
				PreviousGeneration: change.Previous.Generation,
				PID:                change.Current.PID,
				Generation:         change.Current.Generation,
				Ready:              change.Ready,
			})
		}
		if len(lifecycle.Restarted) == 0 {
			err := fmt.Errorf("service %q was not restarted", task)
			lifecycle.Error = err.Error()
			return nil, lifecycle, err
		}
		lifecycle.Success = true
		return nil, lifecycle, nil
	}
	targetName := "__restart_" + task
	wrapped := restartProject{base: p, target: project.Target{Name: targetName, RootTasks: selected}}
	active, runErr := s.startAttachedProjectLocked(ctx, projectName, wrapped, targetName, api.ModeDev, maxParallel, nil)
	unlock()
	var result *api.RunResult
	if runErr == nil {
		<-active.done
		result, runErr = active.result, active.err
	}
	lifecycle.Affected = append(lifecycle.Affected, selected...)
	lifecycle.Success = runErr == nil
	if runErr != nil {
		lifecycle.Error = runErr.Error()
	}
	return result, lifecycle, runErr
}

func waitForTaskTerminalReadiness(ctx context.Context, worktree, instanceID, task string, timeout time.Duration) (api.NodeStatus, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := instance.LoadStatus(worktree, instanceID)
		if err == nil {
			if node, ok := state.Nodes[task]; ok {
				switch node.State {
				case api.StateRunning:
					if !node.Ready {
						break
					}
					return node, nil
				case api.StateFailed, api.StateCanceled, api.StateDegraded, api.StateStopped:
					cause := strings.TrimSpace(node.LastError)
					if cause == "" {
						cause = string(node.State)
					}
					return node, fmt.Errorf("service %q did not become ready: %s", task, cause)
				}
			}
		}
		select {
		case <-ctx.Done():
			return api.NodeStatus{}, ctx.Err()
		case <-deadline.C:
			return api.NodeStatus{}, fmt.Errorf("service %q readiness timed out after %s", task, timeout)
		case <-ticker.C:
		}
	}
}

func restartLifecyclePlan(g *graph.Graph, inst *api.Instance, status *instance.State, selectedTask string, tasks []string) api.LifecyclePlan {
	plan := api.LifecyclePlan{
		RequestedAction:         "restart",
		SelectedTask:            selectedTask,
		TasksToInvalidate:       []string{},
		ProcessesToStop:         []string{},
		TasksToExecute:          append([]string(nil), tasks...),
		ServicesToPreserve:      []string{},
		ServicesToRestart:       []string{},
		ConfirmationRecommended: len(tasks) > 1,
	}
	selected := make(map[string]bool, len(tasks))
	active := make(map[string]bool)
	for name, ref := range inst.Processes {
		if ref.PID > 0 {
			active[name] = true
		}
	}
	if status != nil {
		for name, node := range status.Nodes {
			if def, ok := g.Tasks[name]; ok && project.IsServiceKind(def.Kind) && lifecycleNodeActive(node.State) {
				active[name] = true
			}
		}
	}
	for _, name := range tasks {
		selected[name] = true
		if def, ok := g.Tasks[name]; ok && project.IsServiceKind(def.Kind) {
			plan.ServicesToRestart = append(plan.ServicesToRestart, name)
			if active[name] {
				plan.ProcessesToStop = append(plan.ProcessesToStop, name)
			}
		}
	}
	for name := range active {
		if !selected[name] {
			plan.ServicesToPreserve = append(plan.ServicesToPreserve, name)
		}
	}
	sort.Strings(plan.ProcessesToStop)
	sort.Strings(plan.ServicesToPreserve)
	return plan
}

func (s *Server) retarget(ctx context.Context, target string) (*api.LifecycleResult, error) {
	s.transitionMu.Lock()
	defer s.transitionMu.Unlock()
	return s.retargetLocked(ctx, target)
}

func (s *Server) retargetLocked(ctx context.Context, target string) (*api.LifecycleResult, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("no target selected")
	}
	inst, err := instance.Load(s.worktree, s.instanceID)
	if err != nil {
		return nil, err
	}
	projectName, p, err := s.resolveProject(inst.LastRun.Project)
	if err != nil {
		return nil, err
	}
	execProject, resolvedTarget, err := project.ResolveExecutionProject(p, target)
	if err != nil {
		return nil, err
	}
	g, err := graph.New(execProject.Tasks(), execProject.Targets())
	if err != nil {
		return nil, err
	}
	closure, err := g.TargetClosure(resolvedTarget)
	if err != nil {
		return nil, err
	}
	plan := api.LifecyclePlan{
		RequestedAction:         "retarget",
		SelectedTarget:          resolvedTarget,
		TasksToInvalidate:       []string{},
		ProcessesToStop:         sortedProcessNames(inst.Processes),
		TasksToExecute:          append([]string(nil), closure...),
		ServicesToPreserve:      []string{},
		ServicesToRestart:       serviceNamesInTasks(g, closure),
		ConfirmationRecommended: true,
	}
	lifecycle := &api.LifecycleResult{
		Plan:      plan,
		Affected:  []string{},
		Stopped:   []string{},
		Restarted: []string{},
		Processes: []api.LifecycleProcessChange{},
		Success:   false,
	}
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active != nil && active.target == resolvedTarget && active.projectName == projectName {
		err := fmt.Errorf("target %q is already active; no retarget occurred", resolvedTarget)
		lifecycle.Error = err.Error()
		return lifecycle, err
	}
	_, err = s.startActiveLocked(ctx, projectName, resolvedTarget, inst.LastRun.Mode, inst.LastRun.MaxParallel)
	lifecycle.Affected = append(lifecycle.Affected, closure...)
	lifecycle.Stopped = append(lifecycle.Stopped, plan.ProcessesToStop...)
	lifecycle.Restarted = append(lifecycle.Restarted, plan.ServicesToRestart...)
	lifecycle.Success = err == nil
	if err != nil {
		lifecycle.Error = err.Error()
	}
	return lifecycle, err
}

func sortedProcessNames(processes map[string]api.ProcessRef) []string {
	names := make([]string, 0, len(processes))
	for name, ref := range processes {
		if ref.PID > 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func serviceNamesInTasks(g *graph.Graph, tasks []string) []string {
	names := make([]string, 0)
	for _, name := range tasks {
		if task, ok := g.Tasks[name]; ok && project.IsServiceKind(task.Kind) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (s *Server) listActions(projectName string) (*ActionListResult, error) {
	projectName, p, err := s.resolveProject(projectName)
	if err != nil {
		return nil, err
	}
	return &ActionListResult{
		Project: projectName,
		Actions: project.Actions(p),
	}, nil
}

func (s *Server) runProjectAction(ctx context.Context, projectName, actionID, kind, component string, inputs, env map[string]string) (*ActionRunResult, error) {
	projectName, p, err := s.resolveProject(projectName)
	if err != nil {
		return nil, err
	}
	action, err := project.ResolveAction(p, actionID, kind, component)
	if err != nil {
		return nil, err
	}
	runEnv, normalizedInputs, err := actionInputsToEnv(action, inputs, env)
	if err != nil {
		return &ActionRunResult{
			ActionID:  action.ID,
			Kind:      action.Kind,
			Component: action.Component,
			Status:    "failed",
			Inputs:    normalizedInputs,
		}, err
	}
	s.transitionMu.Lock()
	unlock := sync.OnceFunc(s.transitionMu.Unlock)
	defer unlock()
	inst, err := instance.Load(s.worktree, s.instanceID)
	if os.IsNotExist(err) {
		inst, err = &api.Instance{}, nil
	}
	if err != nil {
		return nil, err
	}
	relaunchTarget := strings.TrimSpace(inst.LastRun.Target)
	relaunchMode := inst.LastRun.Mode
	relaunchMaxParallel := inst.LastRun.MaxParallel
	shouldRelaunch := action.Relaunch == project.ActionRelaunchPreviousTargetAfterSuccess && inst.LastRun.Detached && relaunchTarget != ""

	result := &ActionRunResult{
		RunID:     requestID(),
		ActionID:  action.ID,
		Kind:      action.Kind,
		Component: action.Component,
		Status:    "running",
		Inputs:    normalizedInputs,
	}
	s.publishStatus("running action %s", action.ID)
	if action.Task == "" {
		result.Status = "failed"
		return result, fmt.Errorf("action %q does not define an executable task", action.ID)
	}
	beforeWrites := snapshotActionWriteFiles(s.worktree, action.Effects.Writes)
	active, err := s.startAttachedProjectLocked(ctx, projectName, p, action.Task, api.ModeCI, 0, runEnv)
	if err != nil {
		result.Status = "failed"
		return result, err
	}
	unlock()
	<-active.done
	runResult, err := active.result, active.err
	result.Run = runResult
	result.CreatedFiles = diffCreatedActionFiles(beforeWrites, snapshotActionWriteFiles(s.worktree, action.Effects.Writes))
	if err != nil {
		result.Status = "failed"
		return result, err
	}
	if runResult == nil || !runResult.Success {
		result.Status = "failed"
		return result, fmt.Errorf("action %q did not finish successfully", action.ID)
	}
	result.Status = "succeeded"
	if shouldRelaunch {
		s.transitionMu.Lock()
		defer s.transitionMu.Unlock()
		if s.generation != active.generation {
			// A newer operator selection or stop owns the worktree now.
			return result, nil
		}
		s.publishStatus("relaunching detached target %s after action %s", relaunchTarget, action.ID)
		started, err := s.startActiveLocked(ctx, projectName, relaunchTarget, relaunchMode, relaunchMaxParallel)
		if err != nil {
			result.Status = "succeeded_with_relaunch_failed"
			return result, err
		}
		result.Relaunch = started
		s.publishStatus("detached target %s relaunched", relaunchTarget)
	}
	return result, nil
}

func snapshotActionWriteFiles(worktree string, patterns []string) map[string]bool {
	files := map[string]bool{}
	for _, pattern := range patterns {
		pattern = filepath.ToSlash(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if strings.HasSuffix(pattern, "/**") {
			collectActionWritePath(worktree, strings.TrimSuffix(pattern, "/**"), files)
			continue
		}
		if strings.ContainsAny(pattern, "*?[") {
			matches, _ := filepath.Glob(filepath.Join(worktree, filepath.FromSlash(pattern)))
			for _, match := range matches {
				rel, err := filepath.Rel(worktree, match)
				if err == nil {
					collectActionWritePath(worktree, rel, files)
				}
			}
			continue
		}
		collectActionWritePath(worktree, pattern, files)
	}
	return files
}

func collectActionWritePath(worktree, rel string, out map[string]bool) {
	abs := filepath.Join(worktree, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil {
		return
	}
	if !info.IsDir() {
		out[filepath.ToSlash(filepath.Clean(rel))] = true
		return
	}
	_ = filepath.WalkDir(abs, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(worktree, path)
		if err == nil {
			out[filepath.ToSlash(relPath)] = true
		}
		return nil
	})
}

func diffCreatedActionFiles(before, after map[string]bool) []string {
	created := make([]string, 0)
	for path := range after {
		if !before[path] {
			created = append(created, path)
		}
	}
	sort.Strings(created)
	return created
}

func actionInputsToEnv(action project.Action, inputs, env map[string]string) (map[string]string, map[string]string, error) {
	runEnv := map[string]string{}
	for key, value := range env {
		runEnv[key] = value
	}
	normalized := map[string]string{}
	for key, value := range inputs {
		normalized[key] = value
	}
	for _, input := range action.Inputs {
		value, ok := normalized[input.Name]
		if !ok && input.Default != "" {
			value = input.Default
			ok = true
			normalized[input.Name] = value
		}
		if input.Required && strings.TrimSpace(value) == "" {
			return runEnv, normalized, fmt.Errorf("action %q requires input %q", action.ID, input.Name)
		}
		if input.Env != "" && ok {
			runEnv[input.Env] = actionInputEnvValue(input, value)
		}
	}
	return runEnv, normalized, nil
}

func actionInputEnvValue(input project.ActionInput, value string) string {
	if input.Type != project.ActionInputBool {
		return value
	}
	if isTruthyDaemon(value) {
		return "1"
	}
	return "0"
}

func isTruthyDaemon(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

func executionGraphForProject(p project.Project, target string) (*graph.Graph, string, error) {
	execProject, resolvedTarget, err := project.ResolveExecutionProject(p, target)
	if err != nil {
		return nil, "", err
	}
	g, err := graph.New(execProject.Tasks(), execProject.Targets())
	if err != nil {
		return nil, "", err
	}
	return g, resolvedTarget, nil
}

type restartProject struct {
	base   project.Project
	target project.Target
}

func (p restartProject) Name() string          { return p.base.Name() }
func (p restartProject) Tasks() []project.Task { return p.base.Tasks() }
func (p restartProject) Actions() []project.Action {
	return project.Actions(p.base)
}
func (p restartProject) Targets() []project.Target {
	targets := append([]project.Target(nil), p.base.Targets()...)
	targets = append(targets, p.target)
	return targets
}
func (p restartProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	return p.base.ConfigureInstance(ctx, worktree)
}

func restartClosure(g *graph.Graph, task string, upstream, downstream bool) ([]string, error) {
	if _, ok := g.Tasks[task]; !ok {
		return nil, fmt.Errorf("unknown task %q", task)
	}
	names := []string{task}
	if upstream && downstream {
		up := g.Upstream([]string{task})
		down := g.Downstream(up)
		return g.TopoSort(down)
	}
	if downstream {
		names = g.Downstream([]string{task})
		return g.TopoSort(names)
	}
	if upstream {
		names = g.Upstream([]string{task})
		return g.TopoSort(names)
	}
	return g.TopoSort(names)
}

func downstreamInvalidateTasks(g *graph.Graph, target, selected string) ([]string, error) {
	closure, err := g.TargetClosure(target)
	if err != nil {
		return nil, err
	}
	inClosure := map[string]bool{}
	for _, name := range closure {
		inClosure[name] = true
	}
	selectedTask, ok := g.Tasks[selected]
	if !ok {
		return nil, fmt.Errorf("unknown task %q", selected)
	}
	var candidates []string
	if selectedTask.Kind == project.KindGroup {
		candidates = g.Upstream([]string{selected})
	} else {
		candidates = g.Downstream([]string{selected})
	}
	out := collectInvalidateTasks(g, inClosure, candidates)
	sort.Strings(out)
	return out, nil
}

func collectInvalidateTasks(g *graph.Graph, inClosure map[string]bool, names []string) []string {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		if !inClosure[name] || seen[name] {
			continue
		}
		task := g.Tasks[name]
		if task.Kind == project.KindOnce && (task.Cache || task.Stamp) {
			out = append(out, name)
			seen[name] = true
		}
	}
	return out
}

func writeInvalidateTransition(root, instanceID, target string, g *graph.Graph, invalidated []string) error {
	state, err := instance.LoadStatus(root, instanceID)
	if err != nil {
		return err
	}
	impacted, err := impactedRerunTasks(g, target, invalidated)
	if err != nil {
		return err
	}
	invalidatedSet := make(map[string]bool, len(invalidated))
	for _, name := range invalidated {
		invalidatedSet[name] = true
	}
	impactedSet := make(map[string]bool, len(impacted))
	for _, name := range impacted {
		impactedSet[name] = true
	}
	for name, node := range state.Nodes {
		if invalidatedSet[name] {
			node.State = api.StateDirty
			node.LastRunKey = ""
			node.LastError = ""
			node.PID = 0
			state.Nodes[name] = node
			continue
		}
		if !impactedSet[name] {
			continue
		}
		node.LastError = ""
		node.PID = 0
		switch node.Kind {
		case string(project.KindService), string(project.KindDebugService):
			node.State = api.StatePending
		case string(project.KindGroup), string(project.KindWarmup), string(project.KindOnce):
			if node.State != api.StateDirty {
				node.State = api.StatePending
			}
		}
		state.Nodes[name] = node
	}
	return instance.SaveStatus(root, instanceID, state.Target, state.Mode, state.Nodes)
}

func impactedRerunTasks(g *graph.Graph, target string, invalidated []string) ([]string, error) {
	closure, err := g.TargetClosure(target)
	if err != nil {
		return nil, err
	}
	inClosure := make(map[string]bool, len(closure))
	for _, name := range closure {
		inClosure[name] = true
	}
	downstream := g.Downstream(invalidated)
	out := make([]string, 0, len(downstream))
	seen := make(map[string]bool, len(downstream))
	for _, name := range downstream {
		if !inClosure[name] || seen[name] {
			continue
		}
		out = append(out, name)
		seen[name] = true
	}
	sort.Strings(out)
	return out, nil
}

func applyTemporaryInstanceEnv(root, instanceID string, env map[string]string) (func() error, error) {
	if len(env) == 0 {
		return func() error { return nil }, nil
	}
	inst, err := instance.Load(root, instanceID)
	if err != nil {
		return nil, err
	}
	previous := map[string]string{}
	had := map[string]bool{}
	if inst.Env == nil {
		inst.Env = map[string]string{}
	}
	for key, value := range env {
		previous[key], had[key] = inst.Env[key]
		inst.Env[key] = value
	}
	if err := instance.Save(inst); err != nil {
		return nil, err
	}
	return func() error {
		current, err := instance.Load(root, instanceID)
		if err != nil {
			return fmt.Errorf("restore action environment: %w", err)
		}
		if current.Env == nil {
			current.Env = map[string]string{}
		}
		for key := range env {
			if had[key] {
				current.Env[key] = previous[key]
			} else {
				delete(current.Env, key)
			}
		}
		if err := instance.Save(current); err != nil {
			return fmt.Errorf("restore action environment: %w", err)
		}
		return nil
	}, nil
}

func markStoppedNodes(worktree, instanceID string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	state, err := instance.LoadStatus(worktree, instanceID)
	if err != nil {
		return nil
	}
	for _, name := range names {
		node, ok := state.Nodes[name]
		if !ok {
			continue
		}
		node.State = api.StateStopped
		node.PID = 0
		state.Nodes[name] = node
	}
	return instance.SaveStatus(worktree, instanceID, state.Target, state.Mode, state.Nodes)
}

func markAllStoppedNodes(worktree, instanceID string) error {
	state, err := instance.LoadStatus(worktree, instanceID)
	if err != nil {
		return nil
	}
	for name, node := range state.Nodes {
		if node.PID > 0 && node.Generation > 0 && !instance.ProcessAlive(node.PID) {
			node.State = api.StateStopped
			node.PID = 0
			node.Ready = false
			state.Nodes[name] = node
			continue
		}
		switch node.State {
		case api.StatePending, api.StateStarting, api.StateReady, api.StateRunning, api.StateRestarting, api.StateDegraded, api.StateDirty:
			node.State = api.StateStopped
			node.PID = 0
			state.Nodes[name] = node
		}
	}
	return instance.SaveStatus(worktree, instanceID, state.Target, state.Mode, state.Nodes)
}

// Status retains task PIDs after an interrupted engine loses its live registry.
// Logs and process command lines are diagnostics, not proof of resource ownership.
func additionalRecordedProcessRefs(worktree, instanceID string, inst *api.Instance) map[string]int {
	refs := map[string]int{}
	knownPIDs := map[int]bool{os.Getpid(): true}
	if inst != nil {
		knownPIDs[inst.Daemon.PID] = true
		for _, ref := range inst.Processes {
			knownPIDs[ref.PID] = true
		}
	}
	if state, err := instance.LoadStatus(worktree, instanceID); err == nil {
		names := make([]string, 0, len(state.Nodes))
		for name := range state.Nodes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			node := state.Nodes[name]
			if node.PID > 0 && !knownPIDs[node.PID] {
				refs[name] = node.PID
				knownPIDs[node.PID] = true
			}
		}
	}
	return refs
}

func DaemonStatus(inst *api.Instance) *api.DaemonStatus {
	if inst == nil || inst.Daemon.PID <= 0 {
		return nil
	}
	return &api.DaemonStatus{
		PID:       inst.Daemon.PID,
		Alive:     instance.ProcessAlive(inst.Daemon.PID),
		StartedAt: inst.Daemon.StartedAt,
		LogPath:   inst.Daemon.LogPath,
	}
}

func InstanceURLs(inst *api.Instance) map[string]string {
	if inst == nil {
		return nil
	}
	urls := map[string]string{}
	for _, name := range []string{"backend", "frontend", "app"} {
		if port := inst.Ports[name]; port > 0 {
			urls[name] = fmt.Sprintf("http://localhost:%d", port)
		}
	}
	return urls
}
