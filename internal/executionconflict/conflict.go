// Package executionconflict projects execution admission errors onto public
// JSON types without coupling the file-lock layer to command response types.
package executionconflict

import (
	"errors"

	"github.com/benjaco/devflow/internal/execution"
	"github.com/benjaco/devflow/pkg/api"
)

func Details(err error) *api.ResourceConflict {
	var conflict *execution.ConflictError
	if !errors.As(err, &conflict) {
		return nil
	}
	detail := &api.ResourceConflict{Worktree: conflict.Worktree, RecoveryRequired: conflict.RecoveryRequired}
	if owner := conflict.Owner; owner != nil {
		detail.Worktree = owner.Worktree
		detail.PID = owner.PID
		detail.Target = owner.Target
		detail.Mode = owner.Mode
		detail.Kind = owner.Kind
	}
	return detail
}
