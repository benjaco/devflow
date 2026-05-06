//go:build !windows

package cli

import "syscall"

func execLocalBinary(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}
