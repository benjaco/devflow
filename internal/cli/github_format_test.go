package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/logstream"
	"github.com/benjaco/devflow/pkg/api"
)

func TestGitHubFinalJSONPreservesDecodedEvidenceWithoutLegacyCommands(t *testing.T) {
	result := api.RunResult{Target: "verify", Error: &api.CommandError{Code: "task_failed", Message: "failure ##[group]fake"}, LogTail: []string{"##[endgroup]", "::error::not an annotation", "a literal \\u005b remains literal"}}
	var output, ordinary bytes.Buffer
	if err := githubWriteJSON(&output, result); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(&ordinary, result); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), []byte("##[")) {
		t.Fatalf("legacy runner command remains executable in JSON: %s", &output)
	}
	var decoded, decodedOrdinary any
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ordinary.Bytes(), &decodedOrdinary); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, decodedOrdinary) {
		t.Fatalf("decoded JSON evidence changed: %#v != %#v", decoded, decodedOrdinary)
	}
	if !bytes.Contains(output.Bytes(), []byte("##\\u005bgroup]fake")) || !bytes.HasSuffix(output.Bytes(), []byte("\n")) {
		t.Fatalf("expected JSON-only escaping and ordinary final newline: %s", &output)
	}
	failure := errors.New("JSON writer unavailable")
	if err := githubWriteJSON(githubFailWriter{failure}, result); !errors.Is(err, failure) {
		t.Fatalf("JSON write error = %v", err)
	}
	if err := githubWriteJSON(io.Discard, make(chan int)); err == nil {
		t.Fatal("invalid JSON value was accepted")
	}
}

func TestGitHubFinalHumanResultOmitsReplayedExcerptsAndNeutralizesCommands(t *testing.T) {
	result := api.RunResult{Target: "verify\n::group::injected", InstanceID: "instance##[endgroup]"}
	view := &api.ExecutionView{
		Target: result.Target, InstanceID: result.InstanceID, Details: "issues",
		Error:           &api.CommandError{Code: "task_failed", Message: "error\n::error::injected"},
		Nodes:           []api.NodeStatus{{Name: "node##[group]fake", State: api.StateFailed, LastError: "::endgroup::"}},
		FailureExcerpts: []api.FailureExcerpt{{Node: "node", Lines: []string{"already-grouped-output-marker"}}},
	}
	for _, selectedView := range []*api.ExecutionView{nil, view} {
		var output bytes.Buffer
		if err := githubWriteRunText(&output, &result, selectedView); err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"::", "##[", "already-grouped-output-marker"} {
			if strings.Contains(output.String(), bad) {
				t.Errorf("unsafe or duplicated %q in human result:\n%s", bad, &output)
			}
		}
		if !strings.HasSuffix(output.String(), "\n") || !strings.Contains(output.String(), "\n") {
			t.Fatalf("line boundaries were escaped instead of retained: %q", output.String())
		}
		if selectedView != nil && !strings.Contains(output.String(), "Failure excerpts remain available in retained task logs.") {
			t.Fatalf("missing retained excerpt guidance: %s", &output)
		}
	}
	if len(view.FailureExcerpts) != 1 || view.FailureExcerpts[0].Lines[0] != "already-grouped-output-marker" {
		t.Fatal("human presentation changed stored/JSON excerpt evidence")
	}
	failure := errors.New("human writer unavailable")
	if err := githubWriteRunText(githubFailWriter{failure}, &result, view); !errors.Is(err, failure) {
		t.Fatalf("human write error = %v", err)
	}
}

func githubFormatAttempt(t *testing.T, contents string) api.TaskAttempt {
	t.Helper()
	path := filepath.Join(t.TempDir(), "attempt.log")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	return api.TaskAttempt{Task: "frontend:lint", AttemptID: "attempt-test", State: api.StateDone, Executed: true, LogsComplete: true, LogPath: path, StartedAt: start, FinishedAt: start.Add(1800 * time.Millisecond)}
}

func TestGitHubGroupRetainedLogSafetyAndAttribution(t *testing.T) {
	input := "first\n\n::group::child\n##[group]old child\nline ::error title=bad::not an annotation\n::endgroup::\n##[endgroup]\n\r::warning::still content\npartial"
	attempt := githubFormatAttempt(t, input)
	var output bytes.Buffer
	if err := githubWriteAttemptGroup(context.Background(), &output, attempt); err != nil {
		t.Fatal(err)
	}
	want := "::group::frontend:lint | SUCCESS | 1.8s\nfirst\n\n[child group] child\n[child group] old child\nline : :error title=bad: :not an annotation\n[child endgroup]\n[child endgroup]\n\\r: :warning: :still content\npartial\n::endgroup::\n"
	if output.String() != want {
		t.Fatalf("unexpected group:\n%s\nwant:\n%s", output.String(), want)
	}
	raw, err := os.ReadFile(attempt.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != input {
		t.Fatal("presentation changed retained bytes")
	}
}

func TestGitHubGroupClosesWhenLogCannotBeRead(t *testing.T) {
	attempt := githubFormatAttempt(t, "")
	if err := os.Remove(attempt.LogPath); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := githubWriteAttemptGroup(context.Background(), &output, attempt)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing log error = %v", err)
	}
	if !strings.HasSuffix(output.String(), "::endgroup::\n") {
		t.Fatalf("unclosed group: %q", output.String())
	}
}

func TestGitHubIncompleteGroupUsesAvailableSnapshot(t *testing.T) {
	attempt := githubFormatAttempt(t, "before\n")
	attempt.State = api.StateCanceled
	attempt.FinishedAt = time.Time{}
	attempt.LogsComplete = false
	var output bytes.Buffer
	out := githubAppendDuringReplayWriter{output: &output, path: attempt.LogPath}
	if err := githubWriteAttemptGroup(context.Background(), &out, attempt); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.String(), "::group::frontend:lint | CANCELED | duration unavailable | output incomplete\n") {
		t.Fatalf("incomplete title = %q", output.String())
	}
	if !strings.Contains(output.String(), "before\n::endgroup::\n") || strings.Contains(output.String(), "later\n") {
		t.Fatalf("replay did not keep its bounded snapshot: %q", output.String())
	}
	raw, err := os.ReadFile(attempt.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "before\nlater\n" {
		t.Fatalf("concurrent appended evidence was lost: %q", raw)
	}
}

type githubAppendDuringReplayWriter struct {
	output *bytes.Buffer
	path   string
}

func (w *githubAppendDuringReplayWriter) Write(p []byte) (int, error) {
	if string(p) == "before\n" {
		file, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return 0, err
		}
		_, err = file.WriteString("later\n")
		if err = errors.Join(err, file.Close()); err != nil {
			return 0, err
		}
	}
	return w.output.Write(p)
}

func TestGitHubLogTextNeutralizesEveryWorkflowCommandSyntax(t *testing.T) {
	input := "::GROUP::upper ##[GROUP] old ::add-mask::secret\r::stop-commands::token\n::token:: ##[error]false\x1b"
	got := githubSafeLogText(input)
	for _, unsafe := range []string{"::", "##[", "\r", "\n", "\x1b"} {
		if strings.Contains(got, unsafe) {
			t.Errorf("retained command delimiter/control %q in %q", unsafe, got)
		}
	}
	if !strings.Contains(got, "\\r") || !strings.Contains(got, "\\n") {
		t.Fatalf("control boundaries lost in %q", got)
	}
}

func TestGitHubEmptyAttemptExplainsCacheAndNoOutput(t *testing.T) {
	for _, state := range []api.NodeState{api.StateDone, api.StateCached, api.StateCanceled, api.StateStopped} {
		t.Run(string(state), func(t *testing.T) {
			attempt := githubFormatAttempt(t, "")
			attempt.State = state
			var output bytes.Buffer
			if err := githubWriteAttemptGroup(context.Background(), &output, attempt); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "No retained task output.") {
				t.Fatalf("empty group: %q", output.String())
			}
			if state == api.StateCached && !strings.Contains(output.String(), "reused") {
				t.Fatalf("cache explanation missing: %q", output.String())
			}
		})
	}
}

func TestGitHubLargeLogReplayHasBoundedWrites(t *testing.T) {
	line := strings.Repeat("x", 192*1024)
	attempt := githubFormatAttempt(t, strings.Repeat(line+"\n", 64)+"tail")
	out := &githubCountingWriter{}
	if err := githubWriteAttemptGroup(context.Background(), out, attempt); err != nil {
		t.Fatal(err)
	}
	if out.bytes < 12*1024*1024 || out.largest > logstream.MaxLineBytes+128 {
		t.Fatalf("bytes=%d largest write=%d", out.bytes, out.largest)
	}
	// A line larger than the shared log-reader limit remains explicit evidence loss.
	if err := os.WriteFile(attempt.LogPath, []byte(strings.Repeat("x", logstream.MaxLineBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := githubWriteAttemptGroup(context.Background(), io.Discard, attempt); !errors.Is(err, logstream.ErrLineTooLong) {
		t.Fatalf("oversized line error = %v", err)
	}
}

type githubCountingWriter struct{ bytes, largest int }

func (w *githubCountingWriter) Write(p []byte) (int, error) {
	w.bytes += len(p)
	w.largest = max(w.largest, len(p))
	return len(p), nil
}

type githubFailWriter struct{ err error }

func (w githubFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestGitHubGroupAndAnnotationReturnWriterErrors(t *testing.T) {
	attempt := githubFormatAttempt(t, "line\n")
	failure := errors.New("writer unavailable")
	if err := githubWriteAttemptGroup(context.Background(), githubFailWriter{failure}, attempt); !errors.Is(err, failure) {
		t.Fatalf("group error = %v", err)
	}
	attempt.State = api.StateFailed
	if err := githubWriteFailureAnnotation(githubFailWriter{failure}, attempt); !errors.Is(err, failure) {
		t.Fatalf("annotation error = %v", err)
	}
}

func TestGitHubGeneratedCommandEscapingAndActualFailure(t *testing.T) {
	attempt := githubFormatAttempt(t, "not an error\n")
	attempt.Task = "task%\r\n,:name"
	attempt.State = api.StateFailed
	attempt.LastError = "compiler rejected 50%\r\n::error:: forged"
	var output bytes.Buffer
	if err := githubWriteAttemptGroup(context.Background(), &output, attempt); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.String(), "::group::task%25%0D%0A,:name | FAILED | 1.8s\n") {
		t.Fatalf("title escaping = %q", output.String())
	}
	output.Reset()
	if err := githubWriteFailureAnnotation(&output, attempt); err != nil {
		t.Fatal(err)
	}
	want := "::error title=task%25%0D%0A%2C%3Aname::compiler rejected 50%25%0D%0A::error:: forged; see retained task logs.\n"
	if output.String() != want {
		t.Fatalf("annotation = %q want %q", output.String(), want)
	}
	for _, state := range []api.NodeState{api.StateDone, api.StateStopped, api.StateCanceled, api.StateBlocked, api.StateRunning} {
		attempt.State = state
		output.Reset()
		if err := githubWriteFailureAnnotation(&output, attempt); err != nil {
			t.Fatal(err)
		}
		if output.Len() != 0 {
			t.Fatalf("state %s emitted an error: %s", state, output.String())
		}
	}
}

func TestGitHubStatesAndRecordedDurations(t *testing.T) {
	for state, want := range map[api.NodeState]string{api.StateDone: "SUCCESS", api.StateFailed: "FAILED", api.StateCached: "CACHED", api.StateSkipped: "SKIPPED", api.StateBlocked: "BLOCKED", api.StateCanceled: "CANCELED", api.StateStopped: "STOPPED", api.StateRunning: "RUNNING", api.StateReady: "READY"} {
		if got := githubTaskStatus(state); got != want {
			t.Errorf("%s => %s want %s", state, got, want)
		}
	}
	attempt := githubFormatAttempt(t, "")
	if got := githubAttemptDuration(attempt); got != "1.8s" {
		t.Fatalf("recorded duration = %q", got)
	}
	attempt.FinishedAt = time.Time{}
	if got := githubAttemptDuration(attempt); got != "duration unavailable" {
		t.Fatalf("unfinished duration = %q", got)
	}
	attempt.FinishedAt = attempt.StartedAt.Add(-time.Second)
	if got := githubAttemptDuration(attempt); got != "duration unavailable" {
		t.Fatalf("negative duration = %q", got)
	}
	attempt.FinishedAt = attempt.StartedAt
	if got := githubAttemptDuration(attempt); got != "1ms" {
		t.Fatalf("coarse clock duration = %q", got)
	}
}

func TestGitHubSummaryUsesFinalOutcomeAndEscapesCells(t *testing.T) {
	attempt := githubFormatAttempt(t, "")
	attempt.Task = "task|<b>&`[link](x)\nnext"
	attempt.State = api.StateDone
	result := api.RunResult{Target: "verify", Success: false, Error: &api.CommandError{Code: "repository_repair_failed", Message: "repair failed"}, DurationMs: 2200, Nodes: []api.NodeStatus{
		{Name: attempt.Task, AttemptID: attempt.AttemptID, State: api.StateDone, DurationMs: 999999},
		{Name: "alias", Kind: "group", State: api.StateDone},
		{Name: "dependent", State: api.StateSkipped},
		{Name: "blocked", State: api.StateBlocked},
		{Name: "stopped-service", State: api.StateStopped},
	}}
	record := api.RunRecord{RunID: "run-test", State: api.RunFailed, Attempts: []api.TaskAttempt{attempt}}
	var output bytes.Buffer
	if err := githubWriteSummary(&output, record, result); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Overall result: **FAILED**", "repair failed", "| Task | Final state | Duration |", "1.8s", "| alias | SUCCESS | duration unavailable |", "| dependent | SKIPPED | duration unavailable |", "| blocked | BLOCKED | duration unavailable |", "| stopped-service | STOPPED | duration unavailable |", "5 nodes", "SUCCESS: 2", "SKIPPED: 1", "BLOCKED: 1", "STOPPED: 1"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("summary missing %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "<b>") || strings.Contains(output.String(), "[link](x)") || strings.Contains(output.String(), "task|") || strings.Contains(output.String(), "999.999s") {
		t.Fatalf("unsafe or non-authoritative summary: %s", output.String())
	}
}

func TestGitHubSummaryBoundsRowsAndReportsExactCounts(t *testing.T) {
	nodes := make([]api.NodeStatus, 10005)
	for i := range nodes {
		nodes[i] = api.NodeStatus{Name: fmt.Sprintf("%05d-%s", i, strings.Repeat("<&|", 300)), State: api.StateCached}
	}
	var output bytes.Buffer
	if err := githubWriteSummary(&output, api.RunRecord{State: api.RunSucceeded}, api.RunResult{Success: true, Nodes: nodes}); err != nil {
		t.Fatal(err)
	}
	if output.Len() > 256*1024 {
		t.Fatalf("unbounded summary %d bytes", output.Len())
	}
	for _, want := range []string{"10005 nodes", "CACHED: 10005", "9805 rows omitted", "truncated"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestGitHubSummaryAppendOptionalAndErrors(t *testing.T) {
	result := api.RunResult{Success: true}
	if err := githubAppendSummary("", api.RunRecord{}, result); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(path, []byte("Existing summary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := githubAppendSummary(path, api.RunRecord{}, result); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("Existing summary\n")) || !bytes.Contains(data, []byte("Overall result:")) {
		t.Fatalf("append lost content: %q", data)
	}
	if err := githubAppendSummary(filepath.Dir(path), api.RunRecord{}, result); err == nil {
		t.Fatal("directory accepted as summary")
	}
	failure := errors.New("summary unavailable")
	if err := githubWriteSummary(githubFailWriter{failure}, api.RunRecord{}, result); !errors.Is(err, failure) {
		t.Fatalf("summary error = %v", err)
	}
	before := bytes.Repeat([]byte("x"), githubStepSummaryMaxBytes)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := githubAppendSummary(path, api.RunRecord{}, result); err == nil {
		t.Fatal("oversized combined step summary was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("rejected append changed existing summary")
	}
}
