package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

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
	return worktree, path
}
