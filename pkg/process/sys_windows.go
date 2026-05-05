//go:build windows

package process

import (
	"os/exec"
	"strconv"
)

func prepareCmd(cmd *exec.Cmd) {}

func terminateCmd(cmd *exec.Cmd) error {
	return killProcessTree(cmd)
}

func killCmd(cmd *exec.Cmd) error {
	return killProcessTree(cmd)
}

func killProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
