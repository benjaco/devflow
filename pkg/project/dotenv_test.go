package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvParsesQuotedValuesAndComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(""+
		"FOO=bar\n"+
		"BAR=\"quoted value\"\n"+
		"BAZ='single quoted'\n"+
		"QUX=value # comment\n"+
		"export ZIP=zap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := LoadDotEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if env["FOO"] != "bar" {
		t.Fatalf("unexpected FOO: %q", env["FOO"])
	}
	if env["BAR"] != "quoted value" {
		t.Fatalf("unexpected BAR: %q", env["BAR"])
	}
	if env["BAZ"] != "single quoted" {
		t.Fatalf("unexpected BAZ: %q", env["BAZ"])
	}
	if env["QUX"] != "value" {
		t.Fatalf("unexpected QUX: %q", env["QUX"])
	}
	if env["ZIP"] != "zap" {
		t.Fatalf("unexpected ZIP: %q", env["ZIP"])
	}
}

func TestLoadOptionalDotEnvInWorktreeAndMergeEnvMaps(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, ".env"), []byte("FROM_DOTENV=yes\nDATABASE_URL=wrong\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := LoadOptionalDotEnvInWorktree(worktree, ".env")
	if err != nil {
		t.Fatal(err)
	}
	merged := MergeEnvMaps(env, map[string]string{"DATABASE_URL": "right", "ADDED": "ok"})
	if merged["FROM_DOTENV"] != "yes" {
		t.Fatalf("unexpected FROM_DOTENV: %q", merged["FROM_DOTENV"])
	}
	if merged["DATABASE_URL"] != "right" {
		t.Fatalf("expected override to win, got %q", merged["DATABASE_URL"])
	}
	if merged["ADDED"] != "ok" {
		t.Fatalf("unexpected ADDED: %q", merged["ADDED"])
	}
}
