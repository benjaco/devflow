package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

func TestResultDetailsHealthyStatusIsBounded(t *testing.T) {
	root := t.TempDir()
	inst, err := instance.Resolve(root, "details")
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]api.NodeStatus{}
	for i := 0; i < 2000; i++ {
		name := fmt.Sprintf("check-%04d", i)
		nodes[name] = api.NodeStatus{Name: name, Kind: "once", State: api.StateDone}
	}
	if err := instance.SaveStatus(root, inst.ID, "verify", api.ModeCI, nodes); err != nil {
		t.Fatal(err)
	}
	for _, details := range []string{"summary", "issues", "full", ""} {
		t.Run(details, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := []string{"status", "--worktree", root, "--json"}
			if details != "" {
				args = append(args, "--details", details)
			}
			if err := (&App{Stdout: &stdout, Stderr: &stderr}).Run(args); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Details string
				Counts  struct{ Nodes int }
				Nodes   []api.NodeStatus
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if details == "full" || details == "" {
				if len(result.Nodes) != len(nodes) {
					t.Fatalf("full view lost nodes: %d", len(result.Nodes))
				}
			} else if result.Details != details || result.Counts.Nodes != len(nodes) || len(result.Nodes) != 0 || stdout.Len() > 4096 {
				t.Fatalf("healthy compact status: %s", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("status stderr: %s", stderr.String())
			}
		})
	}
}

func TestResultDetailsRunPreservesRetainedEvidence(t *testing.T) {
	for _, details := range []string{"summary", "issues"} {
		t.Run(details, func(t *testing.T) {
			root := t.TempDir()
			var stdout, stderr bytes.Buffer
			err := (&App{Stdout: &stdout, Stderr: &stderr}).Run([]string{"run", "build", "--ci", "--json", "--project", "cli-fail-project", "--worktree", root, "--details", details, "--progress", "quiet"})
			var result struct {
				RunID, InstanceID, Details string
				Success                    bool
				Error                      *api.CommandError
				Counts                     struct{ Nodes, ProblemNodes int }
				Nodes                      []api.NodeStatus
				FailureExcerpts            []api.FailureExcerpt
				Evidence                   struct{ Run []string }
			}
			if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if err == nil || result.Success || result.RunID == "" || result.Error == nil || result.Error.Message != "boom" || result.Counts.Nodes != 1 || result.Counts.ProblemNodes != 1 || len(result.Nodes) != 1 || result.Nodes[0].AttemptID == "" || len(result.Evidence.Run) == 0 {
				t.Fatalf("compact failure lost evidence: %s (%v)", stdout.String(), err)
			}
			record, err := instance.LoadRun(root, result.InstanceID, result.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Result == nil || record.Result.Success || len(record.Result.Nodes) != 1 || len(record.Result.FailureExcerpts) == 0 {
				t.Fatalf("presentation changed retained evidence: %+v", record)
			}
			if stderr.Len() != 0 {
				t.Fatalf("quiet progress: %s", stderr.String())
			}
			if (len(result.FailureExcerpts) != 0) != (details == "issues") {
				t.Fatalf("issue detail did not control diagnostic samples: %s", stdout.String())
			}
		})
	}
}

func TestProgressControlsTaskLogsIndependently(t *testing.T) {
	for _, progress := range []string{"quiet", "states", "logs"} {
		t.Run(progress, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := (&App{Stdout: &stdout, Stderr: &stderr}).Run([]string{"run", "build", "--ci", "--json", "--project", "cli-fail-project", "--worktree", t.TempDir(), "--progress", progress})
			var result api.RunResult
			if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if err == nil || result.Success || result.RunID == "" || len(result.FailureExcerpts) == 0 {
				t.Fatalf("progress hid final failure: %s (%v)", stdout.String(), err)
			}
			if progress == "quiet" && stderr.Len() != 0 {
				t.Fatalf("quiet emitted progress: %s", stderr.String())
			}
			if progress != "quiet" && !bytes.Contains(stderr.Bytes(), []byte("task fail: failed")) {
				t.Fatalf("missing state progress: %s", stderr.String())
			}
			if got := bytes.Contains(stderr.Bytes(), []byte("implementator failure details")); got != (progress == "logs") {
				t.Fatalf("task logs for %s: %s", progress, stderr.String())
			}
		})
	}
}

func TestResultOptionsRejectBeforeBootstrap(t *testing.T) {
	for _, command := range []string{"run", "status", "flush"} {
		for _, option := range []string{"details", "progress"} {
			t.Run(command+"/"+option, func(t *testing.T) {
				root := t.TempDir()
				if err := os.WriteFile(filepath.Join(root, "devflow.project.go"), []byte("broken adapter"), 0o600); err != nil {
					t.Fatal(err)
				}
				args := []string{command, "--worktree", root, "--json", "--" + option, "bogus"}
				if command == "run" {
					args = append(args, "build", "--ci")
				}
				var stdout, stderr bytes.Buffer
				err := (&App{Stdout: &stdout, Stderr: &stderr}).Run(args)
				var result struct{ Error api.CommandError }
				if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
					t.Fatal(decodeErr)
				}
				if err == nil || result.Error.Code != "invalid_arguments" || result.Error.Phase != "parsing" {
					t.Fatalf("option reached bootstrap: %s (%v)", stdout.String(), err)
				}
				if _, err := os.Stat(filepath.Join(root, ".devflow")); !os.IsNotExist(err) {
					t.Fatalf("invalid option created runtime state: %v", err)
				}
			})
		}
	}
}

func TestResultDetailsBoundsFailuresAndPreservesFlushFreshness(t *testing.T) {
	message := strings.Repeat("€", 10000)
	result := api.FlushResult{RunID: "run", InstanceID: "instance", RequestID: "flush", Synced: true, Success: false, TimedOut: true,
		Error: &api.CommandError{Code: "deadline_exceeded", Phase: "execution", Message: message}}
	for i := 0; i < 2000; i++ {
		name := fmt.Sprintf("task-%04d", i)
		result.Nodes = append(result.Nodes, api.NodeStatus{Name: name, State: api.StateBlocked, LastError: message, RunID: "run", AttemptID: name})
		result.Issues = append(result.Issues, api.FlushIssue{Kind: "not_ready", Task: name, Message: message})
		result.Services = append(result.Services, api.FlushService{Task: name})
	}
	result.Nodes[len(result.Nodes)-1].State = api.StateFailed
	for _, details := range []string{"summary", "issues"} {
		app := &App{details: details}
		view := app.resultView(result).(*api.ExecutionView)
		data, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		if view.Synced == nil || !*view.Synced || view.Success == nil || *view.Success || !view.TimedOut || view.RequestID != "flush" || view.Counts.Nodes != 2000 || view.Counts.Issues != 2000 || view.Counts.UnreadyServices != 2000 {
			t.Fatalf("lost exact counts or freshness: %+v", view)
		}
		if len(data) > 100*1024 || !utf8.Valid(data) || !view.Truncated.Text || !view.Truncated.Nodes || !view.Truncated.Issues || view.Nodes[0].State != api.StateFailed {
			t.Fatalf("unbounded or non-actionable %s view: bytes=%d counts=%+v truncation=%+v", details, len(data), view.Counts, view.Truncated)
		}
		if result.Error.Message != message || len(result.Nodes) != 2000 || result.Nodes[0].LastError != message {
			t.Fatal("presentation mutated source evidence")
		}
	}
}

func TestResultDetailsTextIncludesFailuresAndRetrieval(t *testing.T) {
	app := &App{details: "summary"}
	view := app.resultView(api.FlushResult{RunID: "run", InstanceID: "instance", Target: "dev", Synced: false, Issues: []api.FlushIssue{{Task: "build", Kind: "watch_restart_required", Message: "input changed"}}}).(*api.ExecutionView)
	var text bytes.Buffer
	if err := writeExecutionView(&text, view); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"success=false", "synced=false", "watch_restart_required", "input changed", "devflow runs show run"} {
		if !strings.Contains(text.String(), required) {
			t.Fatalf("text omitted %q: %s", required, text.String())
		}
	}
}
