//go:build !(darwin || linux)

package database

import "os/exec"

func prepareRunnerCmd(cmd *exec.Cmd) {}

func killRunnerCmd(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
