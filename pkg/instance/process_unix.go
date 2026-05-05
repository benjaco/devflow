//go:build !windows

package instance

import (
	"errors"
	"syscall"
)

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func terminateProcessGroup(pid int) error {
	return signalProcessGroup(pid, syscall.SIGTERM)
}

func killProcessGroup(pid int) error {
	return signalProcessGroup(pid, syscall.SIGKILL)
}

func isNoProcess(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}

func signalProcessGroup(pid int, signal syscall.Signal) error {
	if err := syscall.Kill(-pid, signal); err != nil {
		return syscall.Kill(pid, signal)
	}
	return nil
}
