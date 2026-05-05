//go:build !(darwin || linux)

package daemon

import "os/exec"

func prepareDaemonCmd(cmd *exec.Cmd) {}
