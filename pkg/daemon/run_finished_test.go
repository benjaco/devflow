package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func TestDaemonFinishedEventFollowsDurableFinalResult(t *testing.T) {
	for _, scenario := range []string{"success", "cleanup_failure", "watch_cancellation"} {
		t.Run(scenario, func(t *testing.T) {
			worktree := t.TempDir()
			inst, err := instance.Resolve(worktree, "test")
			if err != nil {
				t.Fatal(err)
			}
			record := &api.RunRecord{InstanceID: inst.ID, Project: "fixture", Target: "check", Mode: api.ModeCI}
			if err := instance.CreateRun(worktree, inst.ID, record); err != nil {
				t.Fatal(err)
			}
			record.State = api.RunRunning
			record.Result = &api.RunResult{RunID: record.RunID, InstanceID: inst.ID, Target: record.Target, Mode: record.Mode, Success: true}
			if scenario == "watch_cancellation" {
				record.Result.Success = false
				record.Result.Error = &api.CommandError{Code: "operation_cancelled", Phase: "execution", Message: "context canceled"}
			}
			if err := instance.SaveRun(worktree, inst.ID, record); err != nil {
				t.Fatal(err)
			}
			active := &activeRun{runID: record.RunID, projectName: record.Project, target: record.Target, mode: record.Mode,
				cancel: func() {}, done: make(chan struct{}), operation: &runOperation{id: record.RunID, cancel: func() {}}}
			if scenario != "watch_cancellation" {
				active.result = record.Result
			}
			if scenario == "cleanup_failure" {
				active.err = errors.New("temporary environment restoration failed")
			}
			s := &Server{worktree: worktree, instanceID: inst.ID, active: active, subscribers: map[chan api.Event]bool{}}
			events := s.addSubscriber()
			defer s.removeSubscriber(events)
			// Block event persistence so publication before result persistence would
			// leave the record running and deterministically fail this ordering check.
			s.eventMu.Lock()
			unlock := sync.OnceFunc(s.eventMu.Unlock)
			defer unlock()
			go s.finishActiveRun(active)
			var terminal *api.RunRecord
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				terminal, err = instance.LoadRun(worktree, inst.ID, record.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if terminal.State.Terminal() {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if terminal == nil || !terminal.State.Terminal() {
				t.Fatal("run_finished publication began before the final result became durable")
			}
			unlock()
			select {
			case <-active.done:
			case <-time.After(2 * time.Second):
				t.Fatal("finalization did not finish")
			}
			var finished []api.Event
			for len(events) > 0 {
				event := <-events
				if event.Type == api.EventRunFinished {
					finished = append(finished, event)
				}
			}
			if len(finished) != 1 {
				t.Fatalf("terminal events = %d, want one", len(finished))
			}
			event := finished[0]
			if event.RunID != record.RunID || event.Success == nil || *event.Success != terminal.Result.Success || event.Error != terminal.Result.Error.Error() {
				t.Fatalf("event %+v disagrees with durable result %+v", event, terminal.Result)
			}
		})
	}
}

func TestAttachedStreamDeliversOneFinalEventBeforeResponse(t *testing.T) {
	s := runControlServer(t, "daemon-final-stream-order", func(context.Context, *project.Runtime) error { return nil })
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	if err := clientConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	handlerDone := make(chan struct{})
	go func() {
		s.handleConn(context.Background(), serverConn)
		close(handlerDone)
	}()
	encoder := json.NewEncoder(clientConn)
	if err := encoder.Encode(Request{ID: "stream-order", Action: ActionRun, Target: "check", Mode: api.ModeCI, StreamEvents: true}); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(clientConn)
	var finished []api.Event
	for {
		var message frame
		if err := decoder.Decode(&message); err != nil {
			t.Fatal(err)
		}
		if message.Event != nil && message.Event.Type == api.EventRunFinished {
			finished = append(finished, *message.Event)
			record, err := instance.LoadRun(s.worktree, s.instanceID, message.Event.RunID)
			if err != nil || !record.State.Terminal() {
				t.Fatalf("event preceded durable completion: record=%+v err=%v", record, err)
			}
		}
		if message.Response != nil {
			if !message.Response.OK || message.Response.Run == nil {
				t.Fatalf("run response: %+v", message.Response)
			}
			if len(finished) != 1 || finished[0].RunID != message.Response.Run.RunID {
				t.Fatalf("response overtook terminal event: events=%+v response=%+v", finished, message.Response.Run)
			}
			if err := encoder.Encode(frame{Type: responseAckFrameType, ID: "stream-order"}); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stream handler did not release its subscription")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.subscribers) != 0 {
		t.Fatal("completed event stream retained a subscription")
	}
}
