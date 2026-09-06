//go:build windows

package daemon

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func prepareDaemonCmd(cmd *exec.Cmd) {
	// Client cancellation targets its console group. The daemon owns its work
	// independently and must survive that group's interruption.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}
