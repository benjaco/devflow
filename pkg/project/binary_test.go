package project

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"devflow/pkg/api"
	"devflow/pkg/process"
)

func TestBinaryToolBuildRunAndStart(t *testing.T) {
	worktree := t.TempDir()
	source := filepath.Join(worktree, "tool-src.sh")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nif [ \"$1\" = \"serve\" ]; then\n  echo \"service:$MESSAGE\" >> \"$OUT_FILE\"\n  trap 'exit 0' INT TERM\n  while true; do sleep 1; done\nfi\necho \"run:$1:$MESSAGE\" >> \"$OUT_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	rt := &Runtime{
		Worktree: worktree,
		Instance: &api.Instance{ID: "test-instance"},
		Mode:     api.ModeDev,
		Env:      map[string]string{},
		TaskName: "test",
		LogPath:  filepath.Join(worktree, "task.log"),
	}
	tool := BinaryTool{
		TaskName:    "build_mocktool",
		Description: "Build a mock tool binary",
		Inputs:      Inputs{Files: []string{"tool-src.sh"}},
		Output:      ".devflow/tools/mocktool",
		Build:       processSpec("sh", "-c", "mkdir -p .devflow/tools && cp tool-src.sh .devflow/tools/mocktool && chmod +x .devflow/tools/mocktool"),
	}

	if err := tool.BuildTask().Run(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rt.Abs(".devflow/tools/mocktool")); err != nil {
		t.Fatalf("expected built binary: %v", err)
	}

	outPath := rt.Abs("run.out")
	if err := tool.RunSpec(context.Background(), rt, BinaryExecSpec{
		Args: []string{"hello"},
		Env: map[string]string{
			"OUT_FILE": outPath,
			"MESSAGE":  "world",
		},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "run:hello:world" {
		t.Fatalf("unexpected run output %q", got)
	}

	serviceOut := rt.Abs("service.out")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle, err := tool.StartSpec(ctx, rt, BinaryExecSpec{
		Args: []string{"serve"},
		Env: map[string]string{
			"OUT_FILE": serviceOut,
			"MESSAGE":  "up",
		},
		Grace: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, serviceOut)
	if err := handle.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Wait(); err != nil {
		t.Fatal(err)
	}
	serviceData, err := os.ReadFile(serviceOut)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(serviceData)); got != "service:up" {
		t.Fatalf("unexpected service output %q", got)
	}
}

func processSpec(name string, args ...string) process.CommandSpec {
	return process.CommandSpec{Name: name, Args: args}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for file %s", path)
}
