// Package taskexec shares task callbacks without coupling the engine's
// scheduling and readiness to validation's sandbox and artifact checks.
package taskexec

import (
	"context"

	"github.com/benjaco/devflow/pkg/project"
)

// Run returns the effective runtime so service readiness sees hook changes.
// Hook environment changes are local to this attempt, not sibling tasks.
func Run(ctx context.Context, task project.Task, rt *project.Runtime) (*project.Runtime, error) {
	taskRuntime := rt
	if task.BeforeRun != nil {
		clone := *rt
		clone.Env = rt.CloneEnv()
		taskRuntime = &clone
		if err := task.BeforeRun(ctx, taskRuntime); err != nil {
			return taskRuntime, err
		}
	}
	if task.Run == nil {
		return taskRuntime, nil
	}
	return taskRuntime, task.Run(ctx, taskRuntime)
}
