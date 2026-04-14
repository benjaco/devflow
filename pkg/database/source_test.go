package database

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"devflow/pkg/api"
	"devflow/pkg/process"
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
