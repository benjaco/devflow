package cli

import (
	"context"
	"errors"
	"flag"
	"time"

	"github.com/benjaco/devflow/internal/clierror"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

type executionOptions struct {
	Headless api.HeadlessPolicy
	Timeout  time.Duration
}
type executionFlags struct {
	headless *string
	timeout  *time.Duration
}

func addExecutionFlags(fs *flag.FlagSet) executionFlags {
	return executionFlags{fs.String("headless", "fail", "prompt policy: fail immediately or wait for an explicit response (at most 5m per prompt)"), fs.Duration("timeout", 0, "execution deadline, including queued admission; 0 disables the operation deadline")}
}
func (f executionFlags) options() (executionOptions, error) {
	if *f.headless != "fail" && *f.headless != "wait" {
		return executionOptions{}, clierror.Wrap(errors.New("--headless must be fail or wait"), "invalid_arguments", "parsing")
	}
	if *f.timeout < 0 || (*f.timeout > 0 && *f.timeout < time.Millisecond) {
		return executionOptions{}, clierror.Wrap(errors.New("--timeout must be zero or at least 1ms"), "invalid_arguments", "parsing")
	}
	return executionOptions{api.HeadlessPolicy(*f.headless), *f.timeout}, nil
}

func completeDirectRun(root string, result *api.RunResult, runErr error) error {
	record, err := instance.LoadRun(root, result.InstanceID, result.RunID)
	if err != nil {
		return err
	}
	_, pruneErr := instance.PruneRuns(root, record.InstanceID, instance.DefaultRunRetention)
	pruneErr = clierror.Wrap(pruneErr, "retention_failed", "execution")
	if pruneErr != nil {
		result.Success = false
		result.Error = clierror.Describe(errors.Join(runErr, pruneErr), "retention_failed", "execution")
	}
	record.Result = result
	record.State = api.RunSucceeded
	if !result.Success {
		record.State = api.RunFailed
	}
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		record.State = api.RunCanceled
	}
	record.FinishedAt = time.Now().UTC()
	return errors.Join(pruneErr, instance.SaveRun(root, record.InstanceID, record))
}
