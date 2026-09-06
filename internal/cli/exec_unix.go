//go:build !windows

package cli

import (
	"context"
	"io"
	"syscall"
)

func execLocalBinary(ctx context.Context, path string, argv, env []string, _, _ io.Writer, _ bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return syscall.Exec(path, argv, env)
}
