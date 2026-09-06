package process

import (
	"context"
	"os/exec"
	"time"
)

// CommandContext binds cancellation to the whole command tree. Build tools and
// task commands can spawn children that otherwise retain pipes after the parent exits.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	prepareCmd(cmd)
	cmd.Cancel = func() error { return killCmd(cmd) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}
