//go:build !(darwin || linux || windows)

package daemon

import "os/exec"

func prepareDaemonCmd(cmd *exec.Cmd) {}
