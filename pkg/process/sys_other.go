//go:build !(darwin || linux)

package process

import (
	"os/exec"
)

func prepareCmd(cmd *exec.Cmd) {}

func terminateCmd(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func killCmd(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
