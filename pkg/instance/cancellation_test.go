package instance

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

func TestRunCancellationDoesNotAffectAnotherRun(t *testing.T) {
	worktree := t.TempDir()
	first := createStoredRun(t, worktree, api.RunQueued, time.Time{})
	second := createStoredRun(t, worktree, api.RunRunning, time.Time{})
	firstCtx, cancelFirst, err := ObserveRunCancellation(context.Background(), worktree, first.InstanceID, first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelFirst()
	secondCtx, cancelSecond, err := ObserveRunCancellation(context.Background(), worktree, second.InstanceID, second.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelSecond()
	for range 2 {
		if err := RequestRunCancellation(context.Background(), worktree, first.InstanceID, first.RunID); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-firstCtx.Done():
		if !errors.Is(firstCtx.Err(), context.Canceled) {
			t.Fatal(firstCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("cancel request did not reach its run")
	}
	if err := CheckRunCancellation(secondCtx, worktree, second.InstanceID, second.RunID); err != nil {
		t.Fatalf("another run inherited cancellation: %v", err)
	}
	first.State, first.FinishedAt = api.RunCanceled, time.Now().UTC()
	if err := SaveRun(worktree, first.InstanceID, first); err != nil {
		t.Fatal(err)
	}
	var detail *api.CommandError
	err = RequestRunCancellation(context.Background(), worktree, first.InstanceID, first.RunID)
	if !errors.As(err, &detail) || detail.Code != "run_not_active" {
		t.Fatalf("terminal run cancellation = %v", err)
	}
}

func TestRunCancellationIsObservedBeforeAdmission(t *testing.T) {
	worktree := t.TempDir()
	record := createStoredRun(t, worktree, api.RunQueued, time.Time{})
	if err := RequestRunCancellation(context.Background(), worktree, record.InstanceID, record.RunID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel, err := ObserveRunCancellation(context.Background(), worktree, record.InstanceID, record.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("already canceled run returned an executable context: %v", ctx.Err())
	}
	path, err := cancellationPath(worktree, record.InstanceID, record.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ObserveRunCancellation(context.Background(), worktree, record.InstanceID, "../../escape"); !errors.Is(err, ErrInvalidRunID) {
		t.Fatalf("escaping identity accepted: %v", err)
	}
}

func TestRunCancellationPreservesOperationDeadline(t *testing.T) {
	worktree := t.TempDir()
	record := createStoredRun(t, worktree, api.RunRunning, time.Time{})
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer deadlineCancel()
	ctx, cancel, err := ObserveRunCancellation(deadlineCtx, worktree, record.InstanceID, record.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("deadline became cancellation: %v", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("operation deadline was lost")
	}
}
