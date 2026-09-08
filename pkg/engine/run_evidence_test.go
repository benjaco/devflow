package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func TestLifecycleRestartPreservesCompletedAttempts(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	p := &lifecycleServiceProject{handles: map[string][]*genericServiceHandle{}}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	controller := NewLifecycleController()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := eng.Run(ctx, Request{Target: "dev", Worktree: worktree, Mode: api.ModeDev, LifecycleController: controller})
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("engine did not stop")
		}
	})
	controlCtx, stopControl := context.WithTimeout(ctx, 5*time.Second)
	defer stopControl()
	id, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	attempts := func() []api.TaskAttempt {
		t.Helper()
		runs, err := instance.ListRuns(worktree, id)
		if err != nil || len(runs) != 1 {
			t.Fatalf("load active run: records=%+v err=%v", runs, err)
		}
		var selected []api.TaskAttempt
		for _, attempt := range runs[0].Attempts {
			if attempt.Task == "backend" {
				selected = append(selected, attempt)
			}
		}
		return selected
	}
	if _, err := controller.Restart(controlCtx, "backend"); err != nil {
		t.Fatal(err)
	}
	firstRestart := attempts()
	if len(firstRestart) != 2 || firstRestart[0].State != api.StateStopped || firstRestart[0].FinishedAt.IsZero() || !firstRestart[0].LogsComplete {
		t.Fatalf("restart rewrote its completed predecessor: %+v", firstRestart)
	}
	if firstRestart[0].AttemptID == firstRestart[1].AttemptID || firstRestart[0].LogPath == firstRestart[1].LogPath || firstRestart[1].State != api.StateRunning || firstRestart[1].LogsComplete {
		t.Fatalf("replacement did not get independent running evidence: %+v", firstRestart)
	}
	if _, err := controller.Stop(controlCtx, "backend"); err != nil {
		t.Fatal(err)
	}
	before := attempts()
	if _, err := controller.Restart(controlCtx, "backend"); err != nil {
		t.Fatal(err)
	}
	after := attempts()
	if len(after) != 3 || !reflect.DeepEqual(before, after[:2]) {
		t.Fatalf("starting a stopped service changed earlier attempts: before=%+v after=%+v", before, after)
	}
}

func TestFinalEvidenceSaveFailureChangesSuccessfulResult(t *testing.T) {
	worktree := t.TempDir()
	id, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	record := &api.RunRecord{Project: "evidence-save", Target: "verify", Mode: api.ModeCI}
	if err := instance.CreateRun(worktree, id, record); err != nil {
		t.Fatal(err)
	}
	// Keep the on-disk run readable for prompt closure and retention. Only the
	// final SaveRun fails, because changing a retained selection is forbidden.
	record.Target = "changed-selection"
	session := &runSession{worktree: worktree, record: record}
	result := &api.RunResult{RunID: record.RunID, InstanceID: id, Target: record.Target, Mode: api.ModeCI, Success: true}
	if err := session.finish(result, nil, false); err == nil {
		t.Fatal("expected final evidence save to fail")
	}
	if result.Success || result.Error == nil || result.Error.Code != "evidence_write_failed" || result.Error.Phase != "execution" {
		t.Fatalf("failed terminal save returned successful evidence: %+v", result)
	}
	retained, err := instance.LoadRun(worktree, id, record.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.State.Terminal() || retained.Target != "verify" || retained.Result != nil {
		t.Fatalf("failed terminal save changed retained evidence: %+v", retained)
	}
}

func TestBeginAttemptSaveFailureDoesNotInheritCacheOutcome(t *testing.T) {
	worktree := t.TempDir()
	id, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	previous := api.TaskAttempt{AttemptID: instance.NewAttemptID(), Task: "build", State: api.StateDone, CacheOutcome: "miss", LogsComplete: true}
	record := &api.RunRecord{Project: "attempt-cache-evidence", Target: "verify", Mode: api.ModeWatch, Attempts: []api.TaskAttempt{previous}}
	if err := instance.CreateRun(worktree, id, record); err != nil {
		t.Fatal(err)
	}
	// Reject the new attempt's save before its running transition, without
	// platform-specific permissions or damage to the previous retained record.
	record.Target = "changed-selection"
	session := &runSession{worktree: worktree, record: record}
	inst := &api.Instance{ID: id, Worktree: worktree}
	state := &runState{
		req:  Request{RunID: record.RunID, Target: "verify", Worktree: worktree, Mode: api.ModeWatch, session: session},
		inst: inst,
		status: map[string]api.NodeStatus{"build": {
			Name: "build", AttemptID: previous.AttemptID, State: api.StateDone,
			Cache: &api.CacheTiming{Outcome: "miss"},
		}},
		nodeStarted: map[string]time.Time{},
	}
	runtime := &project.Runtime{Worktree: worktree, Instance: inst}
	eng := &Engine{}
	err = eng.beginAttempt(context.Background(), state, runtime, project.Task{Name: "build", Kind: project.KindOnce})
	if err == nil {
		t.Fatal("expected new attempt save to fail")
	}
	state.setErrorState("build", context.Background(), "", err, 0)
	node := state.status["build"]
	if node.AttemptID == "" || node.AttemptID == previous.AttemptID || len(record.Attempts) != 2 {
		t.Fatalf("failed start did not allocate an independent attempt: node=%+v attempts=%+v", node, record.Attempts)
	}
	if node.Cache != nil {
		t.Errorf("new attempt inherited previous cache timing: %+v", node.Cache)
	}
	if attempt := record.Attempts[1]; attempt.CacheOutcome != "" {
		t.Errorf("new attempt reported a cache decision before cache lookup: %+v", attempt)
	}
	if !reflect.DeepEqual(previous, record.Attempts[0]) {
		t.Fatalf("failed start changed its completed predecessor: before=%+v after=%+v", previous, record.Attempts[0])
	}
}
