package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

// Hosted tests default to ordinary presentation. GitHub-specific fixtures set
// their invocation environment explicitly, including bootstrap subprocesses.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("GITHUB_ACTIONS")
	_ = os.Unsetenv("GITHUB_STEP_SUMMARY")
	os.Exit(m.Run())
}

func TestGitHubPresenterReconcilesOverflowAndDuplicateAttempts(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "attempt.log")
	if err := os.WriteFile(path, []byte("retained-only-marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &githubBlockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	p := newGitHubPresenter(w, "logs", "")
	if cap(p.pending) != githubProgressCapacity {
		t.Fatal("progress queue must have a fixed capacity")
	}
	start := time.Now().UTC().Add(-time.Second)
	record := api.RunRecord{RunID: "run", State: api.RunSucceeded}
	result := api.RunResult{RunID: "run", Success: true, Target: "verify"}
	for i := 0; i < githubProgressCapacity*3; i++ {
		attempt := api.TaskAttempt{Task: "same-task", AttemptID: fmt.Sprintf("attempt-%d", i), LogPath: path, State: api.StateDone, StartedAt: start, FinishedAt: start.Add(time.Second), LogsComplete: true}
		if i%2 == 0 {
			attempt.CacheOutcome = "miss"
		}
		record.Attempts = append(record.Attempts, attempt)
	}
	p.observe(api.Event{Type: api.EventTaskAttemptFinished, RunID: record.RunID, Task: "same-task", Attempt: &record.Attempts[0]})
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first group was not rendered")
	}
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for i := range record.Attempts {
			p.observe(api.Event{Type: api.EventTaskAttemptFinished, RunID: record.RunID, Task: "same-task", Attempt: &record.Attempts[i]})
		}
		for range 10000 {
			p.observe(api.Event{Type: api.EventLogLine, RunID: "run", Task: "same-task", AttemptID: "attempt-0", Line: "must-not-copy-live-output"})
		}
	}()
	select {
	case <-collected:
	case <-time.After(5 * time.Second):
		close(w.release)
		t.Fatal("slow output blocked collection")
	}
	close(w.release)
	p.finish(record, result)
	output := w.output.String()
	if got := strings.Count(output, " | CACHE MISS"); got != len(record.Attempts)/2 {
		t.Errorf("cache-miss tags lost or leaked across attempts: got %d want %d", got, len(record.Attempts)/2)
	}
	for _, marker := range []string{"::group::", "::endgroup::", "retained-only-marker"} {
		if got := strings.Count(output, marker); got != len(record.Attempts) {
			t.Errorf("%q count=%d, want %d independent attempt groups", marker, got, len(record.Attempts))
		}
	}
	if strings.Contains(output, "must-not-copy-live-output") || !strings.Contains(output, "deferred") {
		t.Fatalf("unexpected live replay or missing backpressure diagnostic: %s", output)
	}
}

func TestGitHubCIStampReusePreservesRecordedOutcome(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	executions := 0
	p, root := githubCLIFixture(t, []project.Task{{Name: "setup", Kind: project.KindOnce, Stamp: true, Run: func(_ context.Context, rt *project.Runtime) error {
		executions++
		rt.EmitLogLine("stdout", "setup-execution-marker")
		return nil
	}}}, "setup")
	for i := range 2 {
		var stderr bytes.Buffer
		result, stdout, err := githubCLIRun(context.Background(), p, root, &stderr, "--json")
		if err != nil || !result.Success || executions != 1 {
			t.Fatalf("stamp reuse changed execution: runs=%d err=%v result=%s", executions, err, stdout)
		}
		// The engine records stamp reuse as done; presentation must not invent
		// a different state or replay the earlier attempt's execution output.
		if !strings.Contains(stderr.String(), "::group::setup | SUCCESS |") || strings.Count(stderr.String(), "::group::") != 1 {
			t.Fatalf("missing recorded attempt outcome: %s", &stderr)
		}
		if got := strings.Count(stderr.String(), "setup-execution-marker"); got != 1-i {
			t.Fatalf("stamp reuse replayed execution output %d times: %s", got, &stderr)
		}
	}
}

func TestGitHubPresenterPreservesUnownedProgress(t *testing.T) {
	var output bytes.Buffer
	p := newGitHubPresenter(&output, "logs", "")
	p.observe(api.Event{Type: api.EventLogLine, Task: "setup", Line: "configuring"})
	_, _ = p.Write([]byte("[devflow] repository repair: prepared\n"))
	p.finish(api.RunRecord{}, api.RunResult{})
	for _, line := range []string{"[devflow] setup: configuring\n", "[devflow] repository repair: prepared\n"} {
		if !strings.Contains(output.String(), line) {
			t.Errorf("missing run-level progress %q in %s", line, &output)
		}
	}
	if strings.Contains(output.String(), "[devflow] [devflow]") {
		t.Fatalf("duplicate progress prefix: %s", &output)
	}
}

func TestGitHubFinalOutputKeepsHistoricalCommandsInert(t *testing.T) {
	for _, jsonOut := range []bool{true, false} {
		t.Run(fmt.Sprint(jsonOut), func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", "true")
			t.Setenv("GITHUB_STEP_SUMMARY", "")
			message := "failure with ##[endgroup]\n::error::embedded command"
			p, root := githubCLIFixture(t, []project.Task{{Name: "check", Kind: project.KindOnce, Run: func(_ context.Context, rt *project.Runtime) error {
				rt.EmitLogLine("stderr", "##[group]retained-child-marker")
				return errors.New(message)
			}}}, "check")
			var stderr bytes.Buffer
			args := []string{"--details", "issues"}
			if jsonOut {
				args = append(args, "--json")
			}
			_, stdout, err := githubCLIRun(context.Background(), p, root, &stderr, args...)
			if err == nil {
				t.Fatal("presentation lost the failure")
			}
			before := stderr.Len()
			ReportError(&stderr, err)
			if stderr.Len() != before {
				t.Fatal("entrypoint repeated unsanitized failure text")
			}
			if strings.Contains(stdout, "##[") || strings.Contains(stdout, "\n::") {
				t.Fatalf("final output can execute historical workflow commands: %s", stdout)
			}
			if strings.Count(stderr.String(), "::error title=check::") != 1 || strings.Count(stderr.String(), "::group::check | FAILED |") != 1 {
				t.Fatalf("task failure must have one group and one annotation: %s", &stderr)
			}
			if jsonOut {
				var result api.ExecutionView
				if decodeErr := json.Unmarshal([]byte(stdout), &result); decodeErr != nil || result.Error == nil || !strings.Contains(result.Error.Message, message) {
					t.Fatalf("JSON escaping changed decoded failure: %v, %s", decodeErr, stdout)
				}
			} else if strings.Contains(stdout, "retained-child-marker") {
				t.Fatalf("human final output duplicated task logs: %s", stdout)
			}
		})
	}
}

type githubBlockedWriter struct {
	output  bytes.Buffer
	entered chan struct{}
	release chan struct{}
	blocked bool
}

func (w *githubBlockedWriter) Write(data []byte) (int, error) {
	if !w.blocked && bytes.Contains(data, []byte("::group::")) {
		w.blocked = true
		close(w.entered)
		<-w.release
	}
	return w.output.Write(data)
}
