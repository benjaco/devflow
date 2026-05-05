package database

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

func TestCommandSourcePolicyMergesAdapterAndDatabaseEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test is unix-only")
	}
	worktree := t.TempDir()
	output := filepath.Join(worktree, "source.txt")
	policy := CommandSourcePolicy{
		PolicyName: "clone-dev",
		Spec: process.CommandSpec{
			Name: "sh",
			Args: []string{"-c", "printf '%s|%s|%s' \"$REMOTE_URL\" \"$PGDATABASE\" \"$DATABASE_URL\" > \"$OUT_FILE\""},
		},
	}
	db := api.DBInstance{
		Name: "app_wt_abc",
		URL:  "postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
		Host: "127.0.0.1",
		Port: 55432,
		User: "devflow",
	}
	err := policy.PrepareBase(context.Background(), db, PrepareOptions{
		Worktree: worktree,
		Env: map[string]string{
			"REMOTE_URL": "postgres://remote/dev",
			"OUT_FILE":   output,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(data))
	want := "postgres://remote/dev|app_wt_abc|postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable"
	if got != want {
		t.Fatalf("unexpected command source output %q, want %q", got, want)
	}
}

func TestPostgresDumpSourcePolicyClonesRemoteIntoLocalDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test is unix-only")
	}
	worktree := t.TempDir()
	binDir := filepath.Join(worktree, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteExecutable(t, filepath.Join(binDir, "pg_dump"), `#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in
    -f) dump_file="$2"; shift 2 ;;
    *) remote_url="$1"; shift ;;
  esac
done
printf 'dump:%s' "$remote_url" > "$dump_file"
`)
	mustWriteExecutable(t, filepath.Join(binDir, "psql"), `#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in
    -f) dump_file="$2"; shift 2 ;;
    *) shift ;;
  esac
done
cat "$dump_file" > "$OUT_FILE"
printf '%s' "$DATABASE_URL" >> "$OUT_FILE.url"
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output := filepath.Join(worktree, "dump.txt")
	policy := PostgresDumpSourcePolicy{
		PolicyName: "clone-dev",
		RemoteURL:  "postgres://remote/dev",
	}
	db := api.DBInstance{
		Name: "app_wt_abc",
		URL:  "postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
		Host: "127.0.0.1",
		Port: 55432,
		User: "devflow",
	}
	if err := policy.PrepareBase(context.Background(), db, PrepareOptions{
		Worktree: worktree,
		Env:      map[string]string{"OUT_FILE": output},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "dump:postgres://remote/dev" {
		t.Fatalf("unexpected dump output %q", string(data))
	}
	url, err := os.ReadFile(output + ".url")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(url)) != db.URL {
		t.Fatalf("unexpected local database url %q", string(url))
	}
}

func TestPostgresDumpSourcePolicyFailsWhenPgDumpFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test is unix-only")
	}
	worktree := t.TempDir()
	binDir := filepath.Join(worktree, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteExecutable(t, filepath.Join(binDir, "pg_dump"), "#!/bin/sh\nprintf 'version mismatch\\n' >&2\nexit 42\n")
	psqlMarker := filepath.Join(worktree, "psql-ran.txt")
	mustWriteExecutable(t, filepath.Join(binDir, "psql"), "#!/bin/sh\nprintf ran > "+strconvQuoteForShell(psqlMarker)+"\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	policy := PostgresDumpSourcePolicy{RemoteURL: "postgres://remote/dev"}
	err := policy.PrepareBase(context.Background(), api.DBInstance{
		Name: "app_wt_abc",
		URL:  "postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
		Host: "127.0.0.1",
		Port: 55432,
		User: "devflow",
	}, PrepareOptions{Worktree: worktree})
	if err == nil {
		t.Fatal("expected pg_dump failure to be returned")
	}
	if _, statErr := os.Stat(psqlMarker); !os.IsNotExist(statErr) {
		t.Fatalf("expected psql not to run after pg_dump failure, stat err=%v", statErr)
	}
}

func TestRuntimePrepareProgressLogsOnceAndEmitsEvent(t *testing.T) {
	worktree := t.TempDir()
	logPath := filepath.Join(worktree, "db_prepare.log")
	var events []string
	rt := &project.Runtime{
		Worktree: worktree,
		LogPath:  logPath,
		Instance: &api.Instance{ID: "abc", Worktree: worktree},
		TaskName: "db_prepare",
		EventFn: func(evt api.Event) {
			if evt.Type == api.EventLogLine {
				events = append(events, evt.Stream+": "+evt.Line)
			}
		},
	}

	emitPrepareLine(PrepareOptionsFromRuntime(rt), "stdout", "database: checking cached Prisma migration snapshots")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	line := "stdout: database: checking cached Prisma migration snapshots"
	if got := strings.Count(string(data), line); got != 1 {
		t.Fatalf("expected one progress log line, got %d in:\n%s", got, string(data))
	}
	if got := strings.Count(strings.Join(events, "\n"), line); got != 1 {
		t.Fatalf("expected one progress event, got %d in %#v", got, events)
	}
}

func TestCommandSourcePolicyRuntimePrepareOptionsDoNotDuplicateProcessLogs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test is unix-only")
	}
	worktree := t.TempDir()
	logPath := filepath.Join(worktree, "db_prepare.log")
	var events []string
	rt := &project.Runtime{
		Worktree: worktree,
		LogPath:  logPath,
		Instance: &api.Instance{ID: "abc", Worktree: worktree},
		TaskName: "db_prepare",
		EventFn: func(evt api.Event) {
			if evt.Type == api.EventLogLine {
				events = append(events, evt.Stream+": "+evt.Line)
			}
		},
	}
	policy := CommandSourcePolicy{
		PolicyName: "clone-dev",
		Spec: process.CommandSpec{
			Name: "sh",
			Args: []string{"-c", "printf 'hello\\n'"},
		},
	}

	err := policy.PrepareBase(context.Background(), api.DBInstance{
		Name: "app_wt_abc",
		URL:  "postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
		Host: "127.0.0.1",
		Port: 55432,
		User: "devflow",
	}, PrepareOptionsFromRuntime(rt))
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	line := "stdout: hello"
	if got := strings.Count(string(data), line); got != 1 {
		t.Fatalf("expected one subprocess log line, got %d in:\n%s", got, string(data))
	}
	if got := strings.Count(strings.Join(events, "\n"), line); got != 1 {
		t.Fatalf("expected one subprocess event, got %d in %#v", got, events)
	}
}

func mustWriteExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

func strconvQuoteForShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
