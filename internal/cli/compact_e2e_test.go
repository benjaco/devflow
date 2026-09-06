package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/daemon"
	"github.com/benjaco/devflow/pkg/instance"
)

func TestBootstrapCompactQuietFailureRetainsEvidence(t *testing.T) {
	isolateJSONContractState(t)
	worktree := t.TempDir()
	writeLocalProjectFile(t, worktree, jsonContractProjectSource)
	stdout, stderr, err := runJSONContractCommand(t, worktree, "run", "fail", "--ci", "--details", "summary", "--progress", "quiet", "--json")
	assertJSONContractFailure(t, stdout, stderr, err, "task_failed", "execution")
	var result api.ExecutionView
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if stderr != "" || len(stdout) > 64<<10 || result.Details != "summary" || result.RunID == "" || result.InstanceID == "" || result.Success == nil || *result.Success || len(result.Nodes) != 1 || result.Nodes[0].AttemptID == "" || len(result.Evidence.Run) == 0 {
		t.Fatalf("compiled quiet summary lost its failure evidence: stdout=%s stderr=%s", stdout, stderr)
	}
	// Use the returned command from another directory; evidence recovery must
	// depend on its recorded identity, not the failed command's working directory.
	retained, stderr, err := runJSONContractCommand(t, t.TempDir(), result.Evidence.Run[1:]...)
	if err != nil || stderr != "" {
		t.Fatalf("returned evidence command failed: %v\nstdout=%s\nstderr=%s", err, retained, stderr)
	}
	var record api.RunRecord
	if err := json.Unmarshal([]byte(retained), &record); err != nil {
		t.Fatal(err)
	}
	if record.RunID != result.RunID || record.Result == nil || record.Result.Success || len(record.Attempts) != 1 || record.Attempts[0].AttemptID != result.Nodes[0].AttemptID || !strings.Contains(retained, "failure-evidence-marker") {
		t.Fatalf("compact presentation changed retained evidence: %s", retained)
	}
	// Windows bootstrap forwards child stderr and marks its errors presented.
	// Quiet progress must leave that child's sole text diagnostic visible.
	stdout, stderr, err = runJSONContractCommand(t, worktree, "run", "missing-compact-target", "--ci", "--details", "summary", "--progress", "quiet")
	if err == nil || stdout != "" || strings.Count(stderr, "missing-compact-target") != 1 {
		t.Fatalf("quiet bootstrap lost or duplicated text failure: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
}

func TestCompiledCompactFlushPreservesBoundaryEvidence(t *testing.T) {
	isolateJSONContractState(t)
	worktree := t.TempDir()
	id, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run-" + id + "-0000000000000001"
	startJSONContractPeer(t, worktree, func(req daemon.Request, conn net.Conn) {
		switch req.Action {
		case daemon.ActionPing:
			writeJSONContractPeerResponse(conn, req.ID, map[string]any{"ok": true})
		case daemon.ActionFlush:
			timedOut := req.TimeoutMs == 25
			result := api.FlushResult{RunID: runID, InstanceID: id, Worktree: worktree, Target: req.Target, Mode: api.ModeWatch, RequestID: "flush-observed-boundary", Success: !timedOut, Synced: !timedOut, TimedOut: timedOut}
			response := map[string]any{"ok": !timedOut, "flush": result}
			if timedOut {
				failure := &api.CommandError{Code: "deadline_exceeded", Phase: "execution", Message: "flush observation deadline exceeded"}
				result.Error = failure
				response["flush"], response["error"] = result, failure
			}
			writeJSONContractPeerResponse(conn, req.ID, response)
		}
	})
	for _, tc := range []struct {
		timeout  string
		timedOut bool
	}{{"1s", false}, {"25ms", true}} {
		t.Run(tc.timeout, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := runJSONContractLocal(t, worktree, &stdout, &stderr, "flush", "watch", "--timeout", tc.timeout, "--details", "summary", "--progress", "quiet", "--json")
			if tc.timedOut {
				assertJSONContractFailure(t, stdout.String(), stderr.String(), err, "deadline_exceeded", "execution")
			} else if err != nil {
				t.Fatal(err)
			}
			var result api.ExecutionView
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Details != "summary" || result.RunID != runID || result.InstanceID != id || result.RequestID != "flush-observed-boundary" || result.Synced == nil || *result.Synced == tc.timedOut || result.Success == nil || *result.Success == tc.timedOut || result.TimedOut != tc.timedOut || stderr.Len() != 0 {
				t.Fatalf("compact flush lost observed boundary or timeout evidence: stdout=%s stderr=%s", &stdout, &stderr)
			}
		})
	}
}
