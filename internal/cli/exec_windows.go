//go:build windows

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/benjaco/devflow/pkg/process"
	"golang.org/x/sys/windows"
)

func execLocalBinary(ctx context.Context, path string, argv, env []string, stdout, stderr io.Writer, ownsExecution bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	args := []string(nil)
	if len(argv) > 1 {
		args = argv[1:]
	}
	// The local CLI must first cancel its tasks and publish their result. Unlike
	// a compiler child, it gets a bounded graceful interruption before tree kill.
	cmd := process.CommandContext(context.Background(), path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	cmd.Env = env
	cmd.Stdin = os.Stdin
	output := &childOutputWriter{Writer: stdout}
	cmd.Stdout = output
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	var err, cancellation error
	select {
	case err = <-finished:
	case <-ctx.Done():
		cancellation = ctx.Err()
		if windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid)) == nil {
			timer := time.NewTimer(2 * time.Second)
			select {
			case err = <-finished:
				timer.Stop()
				return localChildResult(err, cancellation, output.written)
			case <-timer.C:
			}
		}
		if ownsExecution {
			_ = cmd.Cancel()
		} else {
			// An attached CLI can have started the independently owned daemon.
			// Tree termination would stop that work after its client disconnects.
			_ = cmd.Process.Kill()
		}
		err = <-finished
	}
	return localChildResult(err, cancellation, output.written)
}

func localChildResult(err, cancellation error, outputWritten bool) error {
	if err == nil {
		if cancellation != nil && !outputWritten {
			return cancellation
		}
		return nil
	}
	code := 1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		code = exitErr.ExitCode()
	}
	// Output ownership survives cancellation: a completed child result must not
	// be followed by another JSON error from the bootstrap parent.
	return childExitError{
		cause:     errors.Join(err, cancellation),
		code:      code,
		presented: cancellation == nil || outputWritten,
	}
}

type childOutputWriter struct {
	io.Writer
	written bool
}

func (w *childOutputWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	w.written = w.written || n > 0
	return n, err
}

type childExitError struct {
	cause     error
	code      int
	presented bool
}

func (e childExitError) Error() string   { return e.cause.Error() }
func (e childExitError) Unwrap() error   { return e.cause }
func (e childExitError) ExitCode() int   { return e.code }
func (e childExitError) Presented() bool { return e.presented }
