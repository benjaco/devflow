package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

func TestLogsTailPreservesBlankLines(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          []string
	}{
		{"empty", "", []string{}},
		{"blank", "\n", []string{""}},
		{"trailing_blanks", "first\n\n\n", []string{"first", "", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worktree, _ := writeCLILog(t, tc.content)
			var output bytes.Buffer
			app := &App{Stdout: &output, Stderr: &bytes.Buffer{}}
			if err := app.Run([]string{"logs", "task", "--worktree", worktree, "--json"}); err != nil {
				t.Fatal(err)
			}
			lines := []string{}
			for _, entry := range decodeJSONLines(t, output.Bytes()) {
				lines = append(lines, entry["line"])
			}
			if !reflect.DeepEqual(lines, tc.want) {
				t.Fatalf("log lines = %q, want %q", lines, tc.want)
			}
		})
	}
}

func TestLogsPropagatesTextWriterFailure(t *testing.T) {
	worktree, _ := writeCLILog(t, "one\n")
	want := errors.New("output closed")
	app := &App{Stdout: logWriterFunc(func([]byte) (int, error) { return 0, want }), Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"logs", "task", "--worktree", worktree}); !errors.Is(err, want) {
		t.Fatalf("writer error = %v, want %v", err, want)
	}
}

func TestLogsTailFollowDoesNotReplayAndIncludesAppendDuringTail(t *testing.T) {
	worktree, path := writeCLILog(t, "one\ntwo\n")
	wantErr := errors.New("stop following")
	lines := []string{}
	writer := logWriterFunc(func(data []byte) (int, error) {
		var entry map[string]string
		if err := json.Unmarshal(data, &entry); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, entry["line"])
		if len(lines) == 1 {
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = file.WriteString("three\n")
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				t.Fatal(errors.Join(err, closeErr))
			}
			return len(data), nil
		}
		return 0, wantErr
	})
	app := &App{Stdout: writer, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"logs", "task", "--worktree", worktree, "--tail", "1", "--follow", "--json"}); !errors.Is(err, wantErr) {
		t.Fatalf("follow error = %v, want %v", err, wantErr)
	}
	if want := []string{"two", "three"}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("followed lines = %q, want %q", lines, want)
	}
}

type logWriterFunc func([]byte) (int, error)

func (f logWriterFunc) Write(data []byte) (int, error) { return f(data) }

func writeCLILog(t *testing.T, content string) (string, string) {
	t.Helper()
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "log-test")
	if err != nil {
		t.Fatal(err)
	}
	path := instance.LogPath(worktree, inst.ID, "task")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := instance.SaveStatus(worktree, inst.ID, "verify", api.ModeCI, map[string]api.NodeStatus{"task": {Name: "task", LogPath: path}}); err != nil {
		t.Fatal(err)
	}
	return worktree, path
}

func TestLogsSelectsRetainedAttemptAfterAnotherRunAndBrokenAdapter(t *testing.T) {
	worktree, record := retainedCLIRun(t, api.RunRunning)
	attemptID := instance.NewAttemptID()
	path, err := instance.AttemptLogPath(worktree, record.InstanceID, record.RunID, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("first evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record.Attempts = []api.TaskAttempt{{Task: "check", AttemptID: attemptID, LogPath: path}}
	if err := instance.SaveRun(worktree, record.InstanceID, record); err != nil {
		t.Fatal(err)
	}
	newer := &api.RunRecord{Project: record.Project, Target: "other", Mode: api.ModeCI}
	if err := instance.CreateRun(worktree, record.InstanceID, newer); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"logs", "check", "--run", record.RunID, "--json", "--worktree", worktree},
		{"logs", "check", "--run", record.RunID, "--attempt", attemptID, "--json", "--worktree", worktree},
	} {
		var output bytes.Buffer
		app := &App{Stdout: &output, Stderr: &bytes.Buffer{}}
		if err := app.Run(args); err != nil {
			t.Fatal(err)
		}
		lines := decodeJSONLines(t, output.Bytes())
		if len(lines) != 1 || lines[0]["line"] != "first evidence" || lines[0]["runId"] != record.RunID || lines[0]["attemptId"] != attemptID {
			t.Fatalf("wrong historical evidence: %+v", lines)
		}
	}
}

func TestFollowedRetainedLogReportsExpiry(t *testing.T) {
	root, record := retainedCLIRun(t, api.RunRunning)
	attemptID := instance.NewAttemptID()
	path, err := instance.AttemptLogPath(root, record.InstanceID, record.RunID, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("retained line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record.Attempts = []api.TaskAttempt{{Task: "check", AttemptID: attemptID, LogPath: path, State: api.StateDone}}
	record.State = api.RunSucceeded
	record.FinishedAt = time.Now().UTC()
	if err := instance.SaveRun(root, record.InstanceID, record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	printed := make(chan struct{})
	var once sync.Once
	var output bytes.Buffer
	app := &App{Context: ctx, Stdout: logWriterFunc(func(data []byte) (int, error) {
		n, err := output.Write(data)
		once.Do(func() { close(printed) })
		return n, err
	}), Stderr: &bytes.Buffer{}}
	done := make(chan error, 1)
	go func() {
		done <- app.Run([]string{"logs", "check", "--run", record.RunID, "--worktree", root, "--follow", "--json"})
	}()
	select {
	case <-printed:
	case <-ctx.Done():
		t.Fatal("log reader did not start")
	}
	// Windows may reject retirement while an observer holds the log. Retry after
	// the reader's next polling boundary; the original record must stay intact.
	for {
		_, pruneErr := instance.PruneRuns(root, record.InstanceID, instance.RunRetention{MaxAge: time.Nanosecond, Now: time.Now().Add(time.Hour)})
		_, loadErr := instance.LoadRun(root, record.InstanceID, record.RunID)
		if errors.Is(loadErr, instance.ErrRunExpired) {
			break
		}
		if loadErr != nil {
			cancel()
			<-done
			t.Fatal(loadErr)
		}
		select {
		case <-ctx.Done():
			cancel()
			<-done
			t.Fatalf("could not retire run: %v", pruneErr)
		case <-time.After(20 * time.Millisecond):
		}
	}
	select {
	case err := <-done:
		if err == nil || ctx.Err() != nil {
			t.Fatalf("expired log follower waited for observer deadline: %v", err)
		}
		decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
		var final map[string]json.RawMessage
		for decoder.More() {
			if err := decoder.Decode(&final); err != nil {
				t.Fatal(err)
			}
		}
		var detail api.CommandError
		if err := json.Unmarshal(final["error"], &detail); err != nil {
			t.Fatal(err)
		}
		if detail.Code != "run_expired" {
			t.Fatalf("wrong expiry error: %+v", detail)
		}
	case <-ctx.Done():
		cancel()
		<-done
		t.Fatal("expired log follower waited for observer deadline")
	}
}
