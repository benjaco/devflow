package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
)

func TestCompactResultBoundsAdapterText(t *testing.T) {
	for _, details := range []string{"summary", "issues"} {
		t.Run(details, func(t *testing.T) {
			app := &App{details: details}
			oversized := strings.Repeat("x", 1<<20)
			view := app.resultView(api.RunResult{Target: oversized, Success: true, ResourceConflict: &api.ResourceConflict{Target: oversized}}).(*api.ExecutionView)
			data, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			if len(data) > 64<<10 || !view.Truncated.Text {
				t.Fatalf("adapter text escaped compact bounds: bytes=%d truncated=%+v", len(data), view.Truncated)
			}
			if (view.Target != "" && view.Target != oversized) || (view.ResourceConflict.Target != "" && view.ResourceConflict.Target != oversized) {
				t.Fatal("compact output returned a clipped target identity")
			}
		})
	}
}

func TestCompactResultBoundsProblemCollections(t *testing.T) {
	for _, tc := range []struct {
		details  string
		maxItems int
		maxBytes int
	}{{"summary", 5, 64 << 10}, {"issues", 50, 256 << 10}} {
		t.Run(tc.details, func(t *testing.T) {
			result := api.FlushResult{Target: "verify", Success: false}
			for i := range 1000 {
				name := fmt.Sprintf("task-%04d", i)
				result.Nodes = append(result.Nodes, api.NodeStatus{Name: name, State: api.StateFailed, LastError: "failed"})
				result.Issues = append(result.Issues, api.FlushIssue{Task: name, Kind: "failed", Message: "failed"})
			}
			view := (&App{details: tc.details}).resultView(result).(*api.ExecutionView)
			data, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			if view.Counts.Nodes != 1000 || view.Counts.ProblemNodes != 1000 || view.Counts.Issues != 1000 {
				t.Fatalf("sampling changed full collection counts: %+v", view.Counts)
			}
			if len(view.Nodes) > tc.maxItems || len(view.Issues) > tc.maxItems || len(data) > tc.maxBytes || !view.Truncated.Nodes || !view.Truncated.Issues {
				t.Fatalf("compact problem collections are unbounded: nodes=%d issues=%d bytes=%d truncated=%+v", len(view.Nodes), len(view.Issues), len(data), view.Truncated)
			}
		})
	}
}

func TestCompactPromptRetainsResponseIdentity(t *testing.T) {
	prompt := api.Prompt{ID: "prompt-AAAAAAAAAAAAAAAAAAAAAAAAAA", RunID: "run-fixture-0000000000000001", Task: "login", AttemptID: "attempt-00000000000000000000000000000000", Kind: "text", Message: strings.Repeat("x", 1024)}
	status := api.StatusResult{PendingPrompts: []api.Prompt{prompt}}
	for _, name := range []string{"a", "b", "c", "d"} {
		status.Nodes = append(status.Nodes, api.NodeStatus{Name: name, State: api.StateFailed, LastError: strings.Repeat("x", 2000)})
	}
	view := (&App{details: "summary"}).resultView(status).(*api.ExecutionView)
	if view.Counts.PendingPrompts != 1 {
		t.Fatalf("compact view lost prompt count: %+v", view.Counts)
	}
	if len(view.PendingPrompts) == 0 {
		if !view.Truncated.PendingPrompts {
			t.Fatal("omitted prompt has no truncation indication")
		}
		return
	}
	got := view.PendingPrompts[0]
	if got.Task != prompt.Task || got.ID != prompt.ID || got.RunID != prompt.RunID || got.AttemptID != prompt.AttemptID || got.Kind != prompt.Kind {
		t.Fatalf("sampled prompt no longer has its exact response identity: %+v", got)
	}
	if !view.Truncated.Text || status.PendingPrompts[0].Message != prompt.Message {
		t.Fatal("compact prompt sampling lost truncation evidence or changed its source")
	}
}

func TestCompactResolutionErrorIsBounded(t *testing.T) {
	cache := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CACHE_HOME", "LOCALAPPDATA"} {
		t.Setenv(name, cache)
	}
	var stdout, stderr bytes.Buffer
	err := (&App{Stdout: &stdout, Stderr: &stderr}).Run([]string{"status", "--json", "--details", "summary", "--instance", strings.Repeat("x", 1<<20)})
	var result struct {
		Success   bool
		Error     *api.CommandError
		Truncated api.ExecutionTruncation
	}
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if err == nil || result.Success || result.Error == nil || result.Error.Code != "unknown_instance" || result.Error.Phase != "resolution" {
		t.Fatalf("compact resolution failure lost its classification: %+v (%v)", result.Error, err)
	}
	if stdout.Len() > 64<<10 || !result.Truncated.Text {
		t.Fatalf("compact resolution failure bypassed text bounds: bytes=%d truncated=%+v", stdout.Len(), result.Truncated)
	}
}
