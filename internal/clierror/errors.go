package clierror

import (
	"context"
	"errors"

	"github.com/benjaco/devflow/internal/executionconflict"
	"github.com/benjaco/devflow/pkg/api"
)

type classified struct {
	detail *api.CommandError
	cause  error
}

func (e *classified) Error() string { return e.cause.Error() }
func (e *classified) Unwrap() error { return e.cause }
func (e *classified) As(target any) bool {
	if p, ok := target.(**api.CommandError); ok {
		*p = e.detail
		return true
	}
	return false
}

// Wrap preserves a more specific source classification across package boundaries.
func Wrap(err error, code, phase string) error {
	if err == nil {
		return nil
	}
	return &classified{detail: Describe(err, code, phase), cause: err}
}

func Describe(err error, code, phase string) *api.CommandError {
	if err == nil {
		return nil
	}
	var detail *api.CommandError
	if errors.As(err, &detail) {
		return &api.CommandError{Code: detail.Code, Phase: detail.Phase, Message: err.Error()}
	}
	switch {
	case executionconflict.Details(err) != nil:
		code, phase = "resource_conflict", "admission"
	case errors.Is(err, context.DeadlineExceeded):
		code = "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		code = "operation_cancelled"
	}
	return &api.CommandError{Code: code, Phase: phase, Message: err.Error()}
}
