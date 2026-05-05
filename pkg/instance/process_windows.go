//go:build windows

package instance

import (
	"errors"
	"os/exec"
	"strconv"

	"golang.org/x/sys/windows"
)

const stillActive = 259

func processAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == stillActive
}

func terminateProcessGroup(pid int) error {
	return terminateProcess(pid)
}

func killProcessGroup(pid int) error {
	return terminateProcess(pid)
}

func isNoProcess(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
}

func terminateProcess(pid int) error {
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run(); err == nil {
		return nil
	}
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.TerminateProcess(handle, 1)
}
