package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

func TestRunsPreserveEarlierAttemptLogs(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	p := &taskLogAttemptProject{}
	eng, err := New(p, root)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Target: "build", Worktree: root, Mode: api.ModeCI}
	first, err := eng.Run(context.Background(), req)
	if err == nil {
		t.Fatal("expected initial task failure")
	}
	path := first.Result.Nodes[0].LogPath
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := eng.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if second.Result.Nodes[0].LogPath == path {
		t.Error("two executions reused the same attempt log path")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("later attempt changed earlier evidence: before=%q after=%q", before, after)
	}
}

func TestRetainedRunsDistinguishExecutionFromCacheReuse(t *testing.T) {
	root, cache := t.TempDir(), t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("input version one"), 0o600); err != nil {
		t.Fatal(err)
	}
	eng, err := New(testProject{}, root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := eng.Run(context.Background(), Request{Target: "build", Worktree: root, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	firstRecord, err := instance.LoadRun(root, first.Instance.ID, first.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if firstRecord.State != api.RunSucceeded || firstRecord.Result == nil || firstRecord.GraphDigest == "" || firstRecord.AdapterVersion == "" || len(firstRecord.Attempts) != 1 {
		t.Fatalf("incomplete evidence: %+v", firstRecord)
	}
	attempt := firstRecord.Attempts[0]
	if !attempt.Executed || attempt.CacheKey == "" || attempt.FinishedAt.IsZero() || attempt.AttemptID != first.Result.Nodes[0].AttemptID {
		t.Fatalf("missing executed provenance: %+v", attempt)
	}
	before, _ := json.Marshal(firstRecord)
	second, err := eng.Run(context.Background(), Request{Target: "build", Worktree: root, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	secondRecord, err := instance.LoadRun(root, second.Instance.ID, second.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	reused := secondRecord.Attempts[0]
	if reused.Executed || reused.State != api.StateCached || reused.CacheKey != attempt.CacheKey || reused.AttemptID == attempt.AttemptID || secondRecord.RunID == firstRecord.RunID {
		t.Fatalf("cache reuse indistinguishable from execution: %+v", secondRecord)
	}
	reloaded, err := instance.LoadRun(root, first.Instance.ID, first.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(reloaded)
	if !bytes.Equal(before, after) {
		t.Fatal("later run changed retained earlier result")
	}
}

func TestDirectRunCancellationIsScopedAndRetained(t *testing.T) {
	root, cache := t.TempDir(), t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	started := make(chan *project.Runtime, 1)
	eng, err := New(ownershipProject{run: func(ctx context.Context, rt *project.Runtime) error { started <- rt; <-ctx.Done(); return ctx.Err() }}, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type executionResult struct {
		out *Outcome
		err error
	}
	done := make(chan executionResult, 1)
	go func() {
		out, err := eng.Run(ctx, Request{Target: "verify", Worktree: root, Mode: api.ModeCI})
		done <- executionResult{out, err}
	}()
	var rt *project.Runtime
	select {
	case rt = <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if rt.RunID == "" || rt.AttemptID == "" {
		t.Fatal("runtime omitted execution identity")
	}
	if err := instance.RequestRunCancellation(ctx, root, rt.Instance.ID, rt.RunID); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("cancel error: %v", result.err)
		}
		record, err := instance.LoadRun(root, rt.Instance.ID, rt.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if record.State != api.RunCanceled || record.Result == nil || record.Result.Success || record.Attempts[0].State != api.StateCanceled {
			t.Fatalf("cancellation evidence: %+v", record)
		}
	case <-ctx.Done():
		t.Fatal("scoped cancellation did not complete")
	}
}

func TestRunIdentityCannotBeReusedForDifferentSelection(t *testing.T) {
	root := t.TempDir()
	id, _, err := instance.IDForWorktree(root)
	if err != nil {
		t.Fatal(err)
	}
	record := &api.RunRecord{Project: "test-project", Target: "another-target", Mode: api.ModeCI}
	if err := instance.CreateRun(root, id, record); err != nil {
		t.Fatal(err)
	}
	eng, err := New(testProject{}, root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = eng.Run(context.Background(), Request{Target: "build", Worktree: root, Mode: api.ModeCI, RunID: record.RunID})
	var detail *api.CommandError
	if !errors.As(err, &detail) || detail.Code != "run_mismatch" {
		t.Fatalf("selection mismatch not rejected: %v", err)
	}
	after, err := instance.LoadRun(root, id, record.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != api.RunQueued || len(after.Attempts) != 0 {
		t.Fatalf("rejected identity was mutated: %+v", after)
	}
}

func TestRunFinishedEventHasDurableTerminalEvidence(t *testing.T) {
	root, cache := t.TempDir(), t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	eng, err := New(&taskLogAttemptProject{}, root)
	if err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEventsLossless()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := eng.Run(ctx, Request{Target: "build", Worktree: root, Mode: api.ModeCI})
		done <- err
	}()
	for {
		select {
		case evt := <-events:
			if evt.Type != api.EventRunFinished {
				continue
			}
			record, err := instance.LoadRun(root, evt.InstanceID, evt.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if !record.State.Terminal() || record.Result == nil || evt.Success == nil || *evt.Success != record.Result.Success || evt.Error != record.Result.Error.Error() {
				t.Fatalf("event preceded durable matching result: event=%+v record=%+v", evt, record)
			}
			if err := <-done; err == nil {
				t.Fatal("expected failing task result")
			}
			return
		case <-ctx.Done():
			t.Fatal("run did not publish terminal evidence")
		}
	}
}

func TestRetentionFailureIsRecordedBeforeTerminalCommit(t *testing.T) {
	root, cache := t.TempDir(), t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	savedPolicy := instance.DefaultRunRetention
	instance.DefaultRunRetention.MaxAge = -time.Second
	t.Cleanup(func() { instance.DefaultRunRetention = savedPolicy })
	eng, err := New(ownershipProject{run: func(context.Context, *project.Runtime) error { return nil }}, root)
	if err != nil {
		t.Fatal(err)
	}
	out, err := eng.Run(context.Background(), Request{Target: "verify", Worktree: root, Mode: api.ModeCI})
	if err == nil || out == nil {
		t.Fatalf("expected retention failure with result: out=%+v err=%v", out, err)
	}
	record, loadErr := instance.LoadRun(root, out.Instance.ID, out.Result.RunID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if record.State != api.RunFailed || record.Result == nil || record.Result.Success || out.Result.Success || record.Result.Error == nil || record.Result.Error.Code != "retention_failed" {
		t.Fatalf("maintenance error arrived after immutable success: outcome=%+v record=%+v", out.Result, record)
	}
	if record.Result.Error.Error() != out.Result.Error.Error() {
		t.Fatalf("returned and retained errors differ: result=%+v record=%+v", out.Result, record)
	}
}
