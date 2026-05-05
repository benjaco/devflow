//go:build darwin || linux

package daemon

import (
	"os/exec"
	"syscall"
)

func prepareDaemonCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
