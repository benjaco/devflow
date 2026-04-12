//go:build darwin || linux

package process

import (
	"os/exec"
	"syscall"
	"time"
)

func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func stopCmd(cmd *exec.Cmd, grace time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid := -cmd.Process.Pid
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		return nil
	}
	time.Sleep(grace)
	_ = syscall.Kill(pgid, syscall.SIGKILL)
	return nil
}
