package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/benjaco/devflow/internal/clierror"
	"github.com/benjaco/devflow/internal/logstream"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

type logSelection struct {
	logstream.LogIdentity
	path     string
	terminal bool
}

func resolveLogSelection(root, instanceID, task, runID, attemptID, cursor string) (logSelection, error) {
	selected := logSelection{LogIdentity: logstream.LogIdentity{InstanceID: instanceID, Task: task, RunID: runID, AttemptID: attemptID}}
	if cursor != "" {
		identity, err := logstream.CursorIdentity(cursor)
		if err != nil || identity.InstanceID != instanceID || identity.Task != task || (runID != "" && identity.RunID != runID) || (attemptID != "" && identity.AttemptID != attemptID) {
			return selected, clierror.Wrap(logstream.ErrInvalidCursor, "invalid_cursor", "parsing")
		}
		selected.LogIdentity = identity
		runID, attemptID = identity.RunID, identity.AttemptID
	}
	if runID != "" {
		record, err := instance.LoadRun(root, instanceID, runID)
		if err != nil {
			return selected, runEvidenceError(err)
		}
		for _, attempt := range record.Attempts {
			if attempt.Task == task && (attemptID == "" || attempt.AttemptID == attemptID) {
				selected.path, selected.AttemptID = attempt.LogPath, attempt.AttemptID
				selected.terminal = record.State.Terminal() || !attempt.FinishedAt.IsZero() || terminalLogState(attempt.State)
			}
		}
		if selected.path == "" {
			return selected, clierror.Wrap(fmt.Errorf("no matching attempt for task %q in run %s", task, runID), "unknown_attempt", "resolution")
		}
		return selected, nil
	}
	if task == "daemon" || task == "tui" {
		path, err := resolveDiagnosticLogPath(root, instanceID, task)
		selected.path = path
		return selected, err
	}
	// One snapshot owns both the pathname and its identity. A second status read
	// can attribute the old log's bytes to an attempt that just replaced it.
	state, err := instance.LoadStatus(root, instanceID)
	if err != nil {
		return selected, err
	}
	node, ok := state.Nodes[task]
	if !ok || node.LogPath == "" {
		return selected, clierror.Wrap(fmt.Errorf("task %q has no attempt log", task), "unknown_attempt", "resolution")
	}
	selected.path, selected.RunID, selected.AttemptID = node.LogPath, node.RunID, node.AttemptID
	selected.terminal = terminalLogState(node.State)
	return selected, nil
}

func terminalLogState(state api.NodeState) bool {
	switch state {
	case api.StateDone, api.StateCached, api.StateFailed, api.StateCanceled, api.StateStopped, api.StateSkipped, api.StateMigrationNeeded:
		return true
	default:
		return false
	}
}

func (a *App) logPage(root string, selected logSelection, cursor string, limit int) error {
	if selected.RunID == "" || selected.AttemptID == "" {
		return clierror.Wrap(errors.New("log pages require a retained task attempt; diagnostic logs use --tail or --follow"), "unknown_attempt", "resolution")
	}
	page, err := logstream.ReadPage(a.context(), selected.path, selected.LogIdentity, cursor, limit, selected.terminal)
	// Retirement can race the bounded read. A discarded page must report expiry
	// rather than leave the caller with a cursor that never referred to retained data.
	if errors.Is(err, logstream.ErrLogResetRequired) || errors.Is(err, os.ErrNotExist) || err == nil {
		if _, loadErr := instance.LoadRun(root, selected.InstanceID, selected.RunID); loadErr != nil {
			return runEvidenceError(loadErr)
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, logstream.ErrInvalidCursor):
			return clierror.Wrap(err, "invalid_cursor", "parsing")
		case errors.Is(err, logstream.ErrLogResetRequired):
			return clierror.Wrap(err, "log_reset_required", "execution")
		case errors.Is(err, logstream.ErrInvalidUTF8):
			return clierror.Wrap(err, "log_invalid_utf8", "execution")
		default:
			return clierror.Wrap(err, "log_read_failed", "execution")
		}
	}
	return a.writeResult(page)
}
