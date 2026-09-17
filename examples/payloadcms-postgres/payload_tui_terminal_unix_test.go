//go:build !windows

package payloadcmspostgres

import (
	"os/exec"
	"syscall"
)

func preparePayloadTUITerminal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
}
