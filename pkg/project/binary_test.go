package project

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/process"
)

func TestBinaryToolBuildRunAndStart(t *testing.T) {
	worktree := t.TempDir()
	source := filepath.Join(worktree, "cmd", "mocktool", "main.go")
	mustWriteProjectTestFile(t, source, `package main

import (
	"fmt"
	"os"
	"os/signal"
)

func appendLine(value string) {
	file, err := os.OpenFile(os.Getenv("OUT_FILE"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	fmt.Fprintln(file, value)
}

func main() {
	message := os.Getenv("MESSAGE")
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		appendLine("service:" + message)
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		<-ch
		return
	}
	arg := ""
	if len(os.Args) > 1 {
		arg = os.Args[1]
	}
	appendLine("run:" + arg + ":" + message)
}
`)

	rt := &Runtime{
		Worktree: worktree,
		Instance: &api.Instance{ID: "test-instance"},
		Mode:     api.ModeDev,
		Env:      map[string]string{},
		TaskName: "test",
		LogPath:  filepath.Join(worktree, "task.log"),
	}
	output := ".devflow/tools/mocktool" + projectTestExeSuffix()
	tool := BinaryTool{
		TaskName:    "build_mocktool",
		Description: "Build a mock tool binary",
		Inputs:      Inputs{Files: []string{"cmd/mocktool/main.go"}},
		Output:      output,
		Build:       process.CommandSpec{Name: "go", Args: []string{"build", "-o", output, "cmd/mocktool/main.go"}},
	}

	if err := tool.BuildTask().Run(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rt.Abs(output)); err != nil {
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

func projectTestExeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func mustWriteProjectTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
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
