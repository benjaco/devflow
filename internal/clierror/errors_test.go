package clierror

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/benjaco/devflow/internal/execution"
)

func TestClassificationSurvivesBoundariesAndCleanup(t *testing.T) {
	cause := errors.New("missing target")
	resolved := Wrap(cause, "unknown_target", "resolution")
	transport := Wrap(fmt.Errorf("daemon: %w", resolved), "daemon_unavailable", "transport")
	detail := Describe(transport, "operation_failed", "execution")
	if detail.Code != "unknown_target" || detail.Phase != "resolution" || detail.Message != "daemon: missing target" || !errors.Is(transport, cause) {
		t.Fatalf("lost source classification/cause: %+v, %v", detail, transport)
	}
	taskFailure := Wrap(errors.New("assertion failed"), "task_failed", "execution")
	detail = Describe(errors.Join(taskFailure, context.Canceled), "operation_failed", "execution")
	if detail.Code != "task_failed" {
		t.Fatalf("cleanup cancellation displaced task failure: %+v", detail)
	}
}

func TestClassificationRecognizesCancellationAndOwnershipWithoutProse(t *testing.T) {
	for _, test := range []struct {
		err         error
		code, phase string
	}{
		{context.Canceled, "operation_cancelled", "bootstrap"},
		{context.DeadlineExceeded, "deadline_exceeded", "bootstrap"},
		{&execution.ConflictError{Cause: errors.New("occupied")}, "resource_conflict", "admission"},
		{errors.New("unknown target: context canceled"), "bootstrap_failed", "bootstrap"},
	} {
		detail := Describe(fmt.Errorf("outer: %w", test.err), "bootstrap_failed", "bootstrap")
		if detail.Code != test.code || detail.Phase != test.phase {
			t.Errorf("classification = %+v; want %s/%s", detail, test.code, test.phase)
		}
	}
}
