package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

func TestDaemonLogsDoNotRequireExecutionSnapshot(t *testing.T) {
	for _, snapshot := range []string{"missing", "corrupt"} {
		t.Run(snapshot, func(t *testing.T) {
			worktree := t.TempDir()
			id, root, err := instance.IDForWorktree(worktree)
			if err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(root, ".devflow", "logs", id, "control.log")
			if err := instance.RecordDaemon(&api.Instance{ID: id, Worktree: root}, os.Getpid(), logPath); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(logPath, []byte("admission failed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			snapshotPath := filepath.Join(root, ".devflow", "state", "instances", id, "instance.json")
			if snapshot == "corrupt" {
				if err := os.WriteFile(snapshotPath, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			app := &App{Stdout: &stdout, Stderr: &stderr}
			if err := app.Run([]string{"logs", "daemon", "--worktree", root, "--json"}); err != nil {
				t.Fatalf("daemon diagnostics unavailable with %s execution snapshot: %v", snapshot, err)
			}
			events := decodeJSONLines(t, stdout.Bytes())
			if len(events) != 1 || events[0]["task"] != "daemon" || events[0]["line"] != "admission failed" {
				t.Fatalf("unexpected daemon logs: %s", stdout.Bytes())
			}
		})
	}
}
