package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

func TestCurrentLogIdentityUsesOneStatusSnapshot(t *testing.T) {
	wt, record := retainedCLIRun(t, api.RunRunning)
	nodes := make([]api.NodeStatus, 2)
	for i := range nodes {
		attemptID := instance.NewAttemptID()
		path, err := instance.AttemptLogPath(wt, record.InstanceID, record.RunID, attemptID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(attemptID+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		nodes[i] = api.NodeStatus{Name: "check", RunID: record.RunID, AttemptID: attemptID, LogPath: path}
	}
	save := func(node api.NodeStatus) error {
		return instance.SaveStatus(wt, record.InstanceID, "verify", api.ModeCI, map[string]api.NodeStatus{"check": node})
	}
	if err := save(nodes[0]); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	stopped := make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				stopped <- nil
				return
			default:
			}
			if err := save(nodes[i%2]); err != nil {
				stopped <- err
				return
			}
		}
	}()
	defer func() {
		close(stop)
		if err := <-stopped; err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < 500; i++ {
		var output bytes.Buffer
		app := &App{Stdout: &output, Stderr: io.Discard}
		if err := app.Run([]string{"logs", "check", "--worktree", wt, "--json"}); err != nil {
			t.Fatal(err)
		}
		var line map[string]string
		if err := json.Unmarshal(output.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line["line"] != line["attemptId"] {
			t.Fatalf("attempt identity mismatch at request %d: %s", i, output.String())
		}
	}
}
