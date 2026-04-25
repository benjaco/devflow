package process

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunCapturesStdoutAndStderr(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "task.log")
	lines := map[string][]string{}
	var mu sync.Mutex
	_, err := Run(context.Background(), CommandSpec{
		Name:    "sh",
		Args:    []string{"-c", "printf 'out\\n'; printf 'err\\n' >&2"},
		LogPath: logPath,
		OnLine: func(stream, line string) {
			mu.Lock()
			defer mu.Unlock()
			lines[stream] = append(lines[stream], line)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines["stdout"]) != 1 || lines["stdout"][0] != "out" {
		t.Fatalf("stdout lines = %v", lines["stdout"])
	}
	if len(lines["stderr"]) != 1 || lines["stderr"][0] != "err" {
		t.Fatalf("stderr lines = %v", lines["stderr"])
	}
}

func TestRunInteractiveAnswersPrompts(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "interactive.log")
	lines := []string{}
	var mu sync.Mutex
	prompts := []PromptRequest{}
	bin := buildPromptCLI(t)
	_, err := Run(context.Background(), CommandSpec{
		Name:        bin,
		Dir:         root,
		LogPath:     logPath,
		Interactive: true,
		Prompts: []PromptSpec{
			{Pattern: "Continue? [y/N]: ", Prompt: "Continue?", Kind: PromptConfirm},
			{Pattern: "Name: ", Prompt: "Name", Kind: PromptText},
		},
		OnPrompt: func(req PromptRequest) (PromptResponse, error) {
			prompts = append(prompts, req)
			switch req.Kind {
			case PromptConfirm:
				return PromptResponse{Value: "y"}, nil
			case PromptText:
				return PromptResponse{Value: "Ada"}, nil
			default:
				return PromptResponse{}, nil
			}
		},
		OnLine: func(stream, line string) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, stream+": "+line)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(prompts))
	}
	if prompts[0].Kind != PromptConfirm || prompts[1].Kind != PromptText {
		t.Fatalf("unexpected prompts: %+v", prompts)
	}
	found := false
	for _, line := range lines {
		if strings.Contains(line, "Hello, Ada") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected greeting in output, got %v", lines)
	}
}

func TestRunTruncatesLogPerAttempt(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "task.log")
	if _, err := Run(context.Background(), CommandSpec{
		Name:    "sh",
		Args:    []string{"-c", "printf 'first\\n'"},
		LogPath: logPath,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), CommandSpec{
		Name:    "sh",
		Args:    []string{"-c", "printf 'second\\n'"},
		LogPath: logPath,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "stdout: second" {
		t.Fatalf("expected truncated current-run log, got %q", got)
	}
}

func TestStartWaitIsCleanAfterIntentionalStop(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "service.log")
	handle, err := Start(context.Background(), CommandSpec{
		Name:    "sh",
		Args:    []string{"-c", "trap 'exit 0' INT TERM; while true; do sleep 1; done"},
		LogPath: logPath,
		Grace:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Wait(); err != nil {
		t.Fatalf("expected intentional stop to wait cleanly, got %v", err)
	}
}

func projectRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func buildPromptCLI(t *testing.T) string {
	t.Helper()
	root := projectRoot(t)
	bin := filepath.Join(t.TempDir(), "promptcli")
	cmd := exec.Command("go", "build", "-o", bin, "./internal/testutil/promptcli")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build prompt cli: %v\n%s", err, string(out))
	}
	return bin
}
