package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func TestServiceReadinessPausesOnlyItsOwnPromptWait(t *testing.T) {
	for _, scenario := range []string{"answer", "other attempt", "operation deadline"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				record := &api.RunRecord{Project: "prompt-readiness", Target: "up", Mode: api.ModeWatch}
				if err := instance.CreateRun(root, "fixture", record); err != nil {
					t.Fatal(err)
				}
				session := &runSession{worktree: root, record: record}
				attempt := "app"
				if scenario == "other attempt" {
					attempt = "unrelated"
				}
				if err := session.setWaiting(attempt, 1); err != nil {
					t.Fatal(err)
				}
				handle := newGenericServiceHandle()
				defer handle.Stop()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				var committed bool
				task := project.Task{ReadyTimeout: 100 * time.Millisecond, Ready: func(ctx context.Context, _ *project.Runtime) error {
					if scenario == "other attempt" {
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				}, AfterReady: func(context.Context, *project.Runtime) error { committed = true; return nil }}
				done := make(chan error, 1)
				go func() {
					done <- (&Engine{}).awaitServiceReady(ctx, &project.Runtime{AttemptID: "app"}, task, handle, session)
				}()
				synctest.Wait()
				if scenario == "answer" {
					time.Sleep(500 * time.Millisecond)
					if committed {
						t.Fatal("HTTP readiness committed while operator still deciding")
					}
					select {
					case err := <-done:
						t.Fatalf("operator wait consumed startup budget: %v", err)
					default:
					}
					if err := session.setWaiting(attempt, -1); err != nil {
						t.Fatal(err)
					}
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					if !committed {
						t.Fatal("answer did not release readiness")
					}
				} else {
					err := <-done
					if scenario == "other attempt" && (err == nil || !strings.Contains(err.Error(), "timed out")) {
						t.Fatalf("unrelated prompt paused startup: %v", err)
					}
					if scenario == "operation deadline" && !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("operator wait disabled operation deadline: %v", err)
					}
					if committed {
						t.Fatal("failed startup committed schema fingerprint")
					}
				}
			})
		})
	}
}

func TestFlushDoesNotReportHealthDuringAServicePrompt(t *testing.T) {
	handle := newGenericServiceHandle()
	defer handle.Stop()
	session := &runSession{promptWaits: map[string]promptWait{"current": {count: 1, since: time.Now()}}}
	state := &runState{inst: &api.Instance{ID: "fixture"}, services: map[string]project.ServiceHandle{"app": handle}}
	service := (&Engine{}).evaluateFlushService(context.Background(), Request{session: session}, &project.Runtime{}, state, project.Task{Name: "app", Kind: project.KindService}, api.NodeStatus{State: api.StateRunning, AttemptID: "current"})
	if service.Ready || !service.Alive || !strings.Contains(service.Error, "operator input") {
		t.Fatalf("pending question reported healthy: %+v", service)
	}
}
