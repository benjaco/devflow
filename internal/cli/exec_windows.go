//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/benjaco/devflow/pkg/process"
	"golang.org/x/sys/windows"
)

func execLocalBinary(ctx context.Context, path string, argv, env []string, stdout, stderr io.Writer, ownsExecution bool, logPath string) error {
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
		writeBootstrapLog(logPath, stderr, "bootstrap_child_start_failed", fmt.Sprintf("error=%q", err))
		return err
	}
	writeBootstrapLog(logPath, stderr, "bootstrap_child_started", fmt.Sprintf("child_pid=%d executable=%q", cmd.Process.Pid, path))
	parentCanceled, terminationScope := false, "none"
	defer func() {
		if cmd.ProcessState == nil {
			writeBootstrapLog(logPath, stderr, "bootstrap_child_exit_observed", fmt.Sprintf("child_pid=%d native_exit=unavailable", cmd.Process.Pid))
			return
		}
		code := uint32(cmd.ProcessState.ExitCode())
		writeBootstrapLog(logPath, stderr, "bootstrap_child_exit_observed", fmt.Sprintf("child_pid=%d native_exit=%d native_hex=0x%08X parent_canceled=%t termination_scope=%s", cmd.Process.Pid, code, code, parentCanceled, terminationScope))
	}()
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	var err, cancellation error
	select {
	case err = <-finished:
	case <-ctx.Done():
		cancellation = ctx.Err()
		parentCanceled = true
		writeBootstrapLog(logPath, stderr, "bootstrap_parent_canceled", fmt.Sprintf("child_pid=%d cause=%q", cmd.Process.Pid, cancellation))
		controlErr := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid))
		writeBootstrapLog(logPath, stderr, "bootstrap_child_interrupt", fmt.Sprintf("child_pid=%d control=CTRL_BREAK_EVENT sent=%t error=%q", cmd.Process.Pid, controlErr == nil, fmt.Sprint(controlErr)))
		if controlErr == nil {
			timer := time.NewTimer(2 * time.Second)
			select {
			case err = <-finished:
				timer.Stop()
				return localChildResult(err, cancellation, output.written)
			case <-timer.C:
			}
		}
		var killErr error
		terminationScope = "child_only"
		if ownsExecution {
			terminationScope = "owned_process_tree"
		}
		writeBootstrapLog(logPath, stderr, "bootstrap_child_termination_requested", fmt.Sprintf("child_pid=%d scope=%s", cmd.Process.Pid, terminationScope))
		if ownsExecution {
			killErr = cmd.Cancel()
		} else {
			// An attached CLI can have started the independently owned daemon.
			// Tree termination would stop that work after its client disconnects.
			killErr = cmd.Process.Kill()
		}
		if killErr != nil {
			writeBootstrapLog(logPath, stderr, "bootstrap_child_termination_failed", fmt.Sprintf("child_pid=%d error=%q", cmd.Process.Pid, killErr))
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
