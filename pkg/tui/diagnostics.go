package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/benjaco/devflow/pkg/instance"
)

const tuiDiagnosticTask = "tui"

type tuiDiagnostics struct {
	path string

	mu      sync.Mutex
	failure error
}

func startTUIDiagnostics(worktree, instanceID string) (*tuiDiagnostics, error) {
	diagnostics := &tuiDiagnostics{path: instance.LogPath(worktree, instanceID, tuiDiagnosticTask)}
	if err := os.MkdirAll(filepath.Dir(diagnostics.path), 0o755); err != nil {
		return nil, fmt.Errorf("create TUI diagnostic directory: %w", err)
	}
	file, err := os.OpenFile(diagnostics.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open TUI diagnostic log %s: %w", diagnostics.path, err)
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("secure TUI diagnostic log %s: %w", diagnostics.path, err)
	}
	if _, err := fmt.Fprintf(file, "%s level=info event=tui_started pid=%d\n", tuiDiagnosticTimestamp(), os.Getpid()); err != nil {
		return nil, fmt.Errorf("initialize TUI diagnostic log %s: %w", diagnostics.path, err)
	}
	// Fatal runtime failures and panics in dependency-owned goroutines cannot be
	// recovered by the TUI event-loop boundary. Keep a second copy of the Go
	// crash report in the per-instance log even while the terminal owns stderr.
	if err := debug.SetCrashOutput(file, debug.CrashOptions{}); err != nil {
		return nil, fmt.Errorf("configure TUI crash output %s: %w", diagnostics.path, err)
	}
	if err := file.Close(); err != nil {
		_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		return nil, fmt.Errorf("close TUI diagnostic log %s: %w", diagnostics.path, err)
	}
	closeFile = false
	return diagnostics, nil
}

func (d *tuiDiagnostics) recordPanic(recovered any, stack []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failure != nil {
		return d.failure
	}

	entry := fmt.Sprintf(
		"%s level=error event=tui_panic panic=%q\n%s",
		tuiDiagnosticTimestamp(),
		boundedDiagnosticText(fmt.Sprint(recovered)),
		stack,
	)
	if len(stack) == 0 || stack[len(stack)-1] != '\n' {
		entry += "\n"
	}
	if err := appendTUIDiagnostic(d.path, entry); err != nil {
		d.failure = fmt.Errorf("devflow TUI crashed and its diagnostic log %s could not be written: %w", d.path, err)
		return d.failure
	}
	d.failure = fmt.Errorf("devflow TUI crashed; diagnostic written to %s", d.path)
	return d.failure
}

func (d *tuiDiagnostics) recordRecoveredPanic(recovered any) error {
	return d.recordPanic(recovered, debug.Stack())
}

func (d *tuiDiagnostics) recordError(cause error) error {
	if cause == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failure != nil {
		return d.failure
	}
	entry := fmt.Sprintf(
		"%s level=error event=tui_error error=%q\n",
		tuiDiagnosticTimestamp(),
		boundedDiagnosticText(cause.Error()),
	)
	if err := appendTUIDiagnostic(d.path, entry); err != nil {
		d.failure = fmt.Errorf("devflow TUI failed: %w; diagnostic log %s could not be written: %v", cause, d.path, err)
		return d.failure
	}
	d.failure = fmt.Errorf("devflow TUI failed: %w; diagnostic written to %s", cause, d.path)
	return d.failure
}

func (d *tuiDiagnostics) recordedFailure() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.failure
}

func (d *tuiDiagnostics) close(runErr error) {
	_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
	status := "ok"
	if runErr != nil {
		status = "error"
	}
	_ = appendTUIDiagnostic(d.path, fmt.Sprintf(
		"%s level=info event=tui_stopped status=%s\n",
		tuiDiagnosticTimestamp(),
		status,
	))
}

func appendTUIDiagnostic(path, entry string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	_, err = file.WriteString(entry)
	return err
}

func boundedDiagnosticText(value string) string {
	const maxBytes = 4096
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	if len(value) <= maxBytes {
		return value
	}
	return value[:maxBytes] + "…"
}

func tuiDiagnosticTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func (d *dashboard) startBackground(run func()) {
	go func() {
		if d.diagnostics == nil {
			run()
			return
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				d.diagnostics.recordRecoveredPanic(recovered)
				d.app.Stop()
			}
		}()
		run()
	}()
}
