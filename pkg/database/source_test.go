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

func TestPostgresDumpSourcePolicyPipesRemoteIntoLocalDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test is unix-only")
	}
	worktree := t.TempDir()
	binDir := filepath.Join(worktree, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteExecutable(t, filepath.Join(binDir, "pg_dump"), "#!/bin/sh\nprintf 'dump:%s' \"$DEVFLOW_REMOTE_DATABASE_URL\"\n")
	mustWriteExecutable(t, filepath.Join(binDir, "psql"), "#!/bin/sh\ncat > \"$OUT_FILE\"\nprintf '%s' \"$DATABASE_URL\" >> \"$OUT_FILE.url\"\n")
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

func mustWriteExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}
