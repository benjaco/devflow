//go:build !(darwin || linux)

package process

import (
	"os/exec"
	"time"
)

func prepareCmd(cmd *exec.Cmd) {}

func stopCmd(cmd *exec.Cmd, grace time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
