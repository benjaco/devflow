package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/benjaco/devflow/pkg/instance"
)

const tuiDiagnosticTask = "tui"

type tuiDiagnostics struct {
	path string

	mu         sync.Mutex
	failure    error
	exitReason string
	sequence   uint64
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
	if _, err := fmt.Fprintf(file, "%s level=info event=tui_started pid=%d ppid=%d instance=%q\n", tuiDiagnosticTimestamp(), os.Getpid(), os.Getppid(), instanceID); err != nil {
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
	d.exitReason = "panic"

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
	d.exitReason = "error"
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

func (d *tuiDiagnostics) recordEvent(format string, args ...any) uint64 {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sequence++
	_ = appendTUIDiagnostic(d.path, fmt.Sprintf("%s level=info %s pid=%d sequence=%d\n", tuiDiagnosticTimestamp(), fmt.Sprintf(format, args...), os.Getpid(), d.sequence))
	return d.sequence
}

func (d *tuiDiagnostics) recordExitRequest(reason string, input uint64, handler string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.exitReason == "" {
		d.exitReason = reason
	}
	// Record before daemon cleanup, which can block after the UI has stopped.
	_ = appendTUIDiagnostic(d.path, fmt.Sprintf("%s level=info event=tui_exit_requested reason=%s input=%d handler=%s user_intent=unknown pid=%d\n", tuiDiagnosticTimestamp(), reason, input, handler, os.Getpid()))
}

func (d *tuiDiagnostics) close(runErr error) {
	_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
	d.mu.Lock()
	defer d.mu.Unlock()
	status := "returned"
	reason := d.exitReason
	if reason == "" {
		reason = "application_returned"
	}
	if runErr != nil {
		status = "error"
		if d.exitReason == "" {
			reason = "error"
		}
	}
	_ = appendTUIDiagnostic(d.path, fmt.Sprintf(
		"%s level=info event=tui_stopped status=%s reason=%s user_intent=unknown pid=%d\n",
		tuiDiagnosticTimestamp(),
		status,
		reason,
		os.Getpid(),
	))
}

func appendTUIDiagnostic(path, entry string) (err error) {
	defer func() {
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "devflow: write TUI diagnostic %s: %v\n", path, err)
		}
	}()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err = file.WriteString(entry); err != nil {
		return err
	}
	return file.Sync()
}

func (d *dashboard) observeControl(event *tcell.EventKey) uint64 {
	control := ""
	switch event.Key() {
	case tcell.KeyEsc:
		control = "escape"
	case tcell.KeyCtrlC:
		control = "ctrl_c"
	default:
		return 0 // Printable keys may be prompt answers, including q.
	}
	modal := "none"
	switch {
	case d.helpOpen:
		modal = "help"
	case d.lifecycleOverlay != lifecycleOverlayNone:
		modal = "lifecycle"
	case d.activeInput:
		modal = "input"
	}
	return d.diagnostics.recordEvent("event=input_observed control=%s modifiers=%d modal=%s focus=%d decoded_at=%s input_origin=unknown user_intent=unknown handler=dashboard.captureKeys", control, event.Modifiers(), modal, d.focusedPane, event.When().UTC().Format(time.RFC3339Nano))
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
