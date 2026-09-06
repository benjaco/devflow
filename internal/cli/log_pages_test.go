package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/logstream"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

func createPagedCLILog(t *testing.T, text string) (string, *api.RunRecord, api.TaskAttempt) {
	t.Helper()
	root, record := retainedCLIRun(t, api.RunRunning)
	attempt := api.TaskAttempt{Task: "check", AttemptID: instance.NewAttemptID(), State: api.StateRunning}
	path, err := instance.AttemptLogPath(root, record.InstanceID, record.RunID, attempt.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	attempt.LogPath = path
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	record.Attempts = []api.TaskAttempt{attempt}
	if err := instance.SaveRun(root, record.InstanceID, record); err != nil {
		t.Fatal(err)
	}
	if err := instance.SaveStatus(root, record.InstanceID, "verify", api.ModeCI, map[string]api.NodeStatus{"check": {Name: "check", State: api.StateRunning, RunID: record.RunID, AttemptID: attempt.AttemptID, LogPath: path}}); err != nil {
		t.Fatal(err)
	}
	return root, record, attempt
}

func readCLIPage(t *testing.T, root string, args ...string) logstream.Page {
	t.Helper()
	var out, stderr bytes.Buffer
	a := &App{Stdout: &out, Stderr: &stderr}
	if err := a.Run(append([]string{"logs", "check", "--json", "--worktree", root}, args...)); err != nil {
		t.Fatalf("page error %v: %s", err, out.String())
	}
	var page logstream.Page
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatalf("single page JSON: %v: %s", err, out.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
	return page
}

func TestLogPagesResumeOriginalAttemptAfterCurrentStatusChanges(t *testing.T) {
	root, record, attempt := createPagedCLILog(t, "first\n€last")
	page := readCLIPage(t, root, "--max-bytes", "6")
	if page.Text != "first\n" || page.AtEnd || page.RunID != record.RunID || page.AttemptID != attempt.AttemptID {
		t.Fatalf("first page: %+v", page)
	}
	if err := instance.SaveStatus(root, record.InstanceID, "other", api.ModeCI, map[string]api.NodeStatus{}); err != nil {
		t.Fatal(err)
	}
	resume := page.NextCursor
	page = readCLIPage(t, root, "--cursor", page.NextCursor)
	if page.Text != "€last" || !page.AtEnd || page.StartOffset != 6 || page.RunID != record.RunID || page.AttemptID != attempt.AttemptID {
		t.Fatalf("resume selected wrong evidence: %+v", page)
	}
	retry := readCLIPage(t, root, "--cursor", resume)
	if retry.Text != page.Text || retry.NextCursor != page.NextCursor {
		t.Fatalf("retry changed retained page: %+v, want %+v", retry, page)
	}
	empty := readCLIPage(t, root, "--cursor", page.NextCursor)
	if empty.Text != "" || !empty.AtEnd || empty.StartOffset != page.EndOffset {
		t.Fatalf("EOF replay: %+v", empty)
	}
	file, err := os.OpenFile(attempt.LogPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("\nnew")
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("append failed: %v, %v", err, closeErr)
	}
	appended := readCLIPage(t, root, "--cursor", empty.NextCursor)
	if appended.Text != "\nnew" || appended.StartOffset != empty.EndOffset || !appended.AtEnd {
		t.Fatalf("append skipped or replayed bytes: %+v", appended)
	}
}

func TestLogPagesStructuredErrors(t *testing.T) {
	for _, scenario := range []string{"size", "cursor", "follow", "tail", "task", "instance", "attempt", "truncate", "replace", "expired", "invalid_utf8"} {
		t.Run(scenario, func(t *testing.T) {
			root, record, attempt := createPagedCLILog(t, "first\nlast\n")
			page := readCLIPage(t, root, "--max-bytes", "6")
			args := []string{"logs", "check", "--json", "--worktree", root, "--cursor", page.NextCursor}
			code, phase := "invalid_arguments", "parsing"
			switch scenario {
			case "size":
				args = append(args, "--max-bytes", "3")
			case "cursor":
				args[len(args)-1] = "broken"
				code = "invalid_cursor"
			case "follow":
				args = append(args, "--follow")
			case "tail":
				args = append(args, "--tail", "1")
			case "task":
				args[1] = "other"
				code = "invalid_cursor"
			case "instance":
				args[4] = t.TempDir()
				code = "invalid_cursor"
			case "attempt":
				args = append(args, "--run", record.RunID, "--attempt", instance.NewAttemptID())
				code = "invalid_cursor"
			case "truncate":
				if err := os.WriteFile(attempt.LogPath, []byte("new"), 0o600); err != nil {
					t.Fatal(err)
				}
				code, phase = "log_reset_required", "execution"
			case "replace":
				if err := os.Rename(attempt.LogPath, attempt.LogPath+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(attempt.LogPath, []byte("first\nlast\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				code, phase = "log_reset_required", "execution"
			case "expired":
				record.State, record.FinishedAt = api.RunSucceeded, time.Now().UTC()
				if err := instance.SaveRun(root, record.InstanceID, record); err != nil {
					t.Fatal(err)
				}
				if _, err := instance.PruneRuns(root, record.InstanceID, instance.RunRetention{MaxAge: time.Nanosecond, Now: time.Now().Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
				code, phase = "run_expired", "resolution"
			case "invalid_utf8":
				if err := os.WriteFile(attempt.LogPath, []byte{0xe2, 0x82}, 0o600); err != nil {
					t.Fatal(err)
				}
				record.Attempts[0].State, record.Attempts[0].FinishedAt = api.StateDone, time.Now().UTC()
				if err := instance.SaveRun(root, record.InstanceID, record); err != nil {
					t.Fatal(err)
				}
				args = []string{"logs", "check", "--json", "--worktree", root, "--run", record.RunID, "--max-bytes", "8"}
				code, phase = "log_invalid_utf8", "execution"
			}
			result, _, err := runEvidenceCLI(t, "", args...)
			if err == nil {
				t.Fatal("expected command failure")
			}
			var detail api.CommandError
			if err := json.Unmarshal(result["error"], &detail); err != nil {
				t.Fatal(err)
			}
			if detail.Code != code || detail.Phase != phase {
				t.Fatalf("error = %+v, want %s/%s", detail, code, phase)
			}
		})
	}
}
