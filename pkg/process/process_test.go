package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/testutil"
)

func TestRunCapturesStdoutAndStderr(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "task.log")
	lines := map[string][]string{}
	var mu sync.Mutex
	testcmd := testutil.BuildTestCommand(t)
	_, err := Run(context.Background(), CommandSpec{
		Name:    testcmd,
		Args:    []string{"emit", "out", "err"},
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

func TestRunCapturesLongOutputLineAndUsesPrivateLogPermissions(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "long-line.log")
	testcmd := testutil.BuildTestCommand(t)
	wantBytes := 256 * 1024
	lines := 0
	_, err := Run(context.Background(), CommandSpec{
		Name:    testcmd,
		Args:    []string{"long-line", fmt.Sprint(wantBytes)},
		LogPath: logPath,
		OnLine: func(stream, line string) {
			if stream != "stdout" || len(line) != wantBytes {
				t.Errorf("unexpected long line stream=%s bytes=%d", stream, len(line))
			}
			lines++
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if lines != 1 {
		t.Fatalf("captured long lines = %d, want 1", lines)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("task log permissions = %03o, want 600", got)
		}
	}
}

func TestRunPropagatesOversizedOutputScannerErrorWithoutStalling(t *testing.T) {
	testcmd := testutil.BuildTestCommand(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Run(ctx, CommandSpec{
		Name: testcmd,
		Args: []string{"long-line", fmt.Sprint(MaxOutputLineBytes + 1024)},
	})
	if err == nil || !strings.Contains(err.Error(), "scan stdout output") || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("expected propagated scanner error, got %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("oversized output stalled until timeout: %v", ctx.Err())
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

func TestInteractivePromptFailureStopsOwnedProcess(t *testing.T) {
	bin := buildPromptCLI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	want := errors.New("interaction requires an explicit response")
	_, err := Run(ctx, CommandSpec{
		Name: bin, Interactive: true,
		Prompts:  []PromptSpec{{Pattern: "Continue? [y/N]: ", Kind: PromptConfirm}},
		OnPrompt: func(PromptRequest) (PromptResponse, error) { return PromptResponse{}, want },
	})
	if !errors.Is(err, want) {
		t.Fatalf("prompt rejection did not stop the child and preserve its error: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("child survived until operation deadline: %v", ctx.Err())
	}
}

func TestSecretPromptResponseHidesSubprocessOutput(t *testing.T) {
	bin := buildPromptCLI(t)
	root := t.TempDir()
	logPath := filepath.Join(root, "secret.log")
	const secret = "private-answer"
	var lines []string
	_, err := Run(context.Background(), CommandSpec{
		Name: bin, Interactive: true, LogPath: logPath,
		Prompts: []PromptSpec{
			{Pattern: "Continue? [y/N]: ", Kind: PromptConfirm},
			{Pattern: "Name: ", Kind: PromptText, Secret: true},
		},
		OnPrompt: func(req PromptRequest) (PromptResponse, error) {
			if req.Kind == PromptConfirm {
				return PromptResponse{Value: "y"}, nil
			}
			if !req.Secret {
				t.Error("secret flag did not reach prompt handler")
			}
			return PromptResponse{Value: secret}, nil
		},
		OnLine: func(_, line string) { lines = append(lines, line) },
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{string(data), strings.Join(lines, "\n")} {
		if strings.Contains(output, secret) || strings.Contains(output, "Hello,") {
			t.Errorf("secret-bearing subprocess output escaped suppression: %q", output)
		}
		if !strings.Contains(output, "[output hidden after secret response]") {
			t.Errorf("missing explanation for hidden output: %q", output)
		}
	}
}

func TestRunInteractiveAnswersRepeatedAlternativePrompts(t *testing.T) {
	root := t.TempDir()
	prompts := []PromptRequest{}
	var mu sync.Mutex
	lines := []string{}
	bin := buildPromptCLI(t)
	_, err := Run(context.Background(), CommandSpec{
		Name:        bin,
		Dir:         root,
		Env:         map[string]string{"PROMPTCLI_REPEAT_CONFIRM": "1"},
		Interactive: true,
		Prompts: []PromptSpec{
			{
				Patterns: []string{
					"Drop field? [y/N]: ",
					"Delete column? [y/N]: ",
				},
				Prompt: "Accept data-loss warning?",
				Kind:   PromptConfirm,
				Repeat: true,
			},
		},
		OnPrompt: func(req PromptRequest) (PromptResponse, error) {
			prompts = append(prompts, req)
			return PromptResponse{Value: "y"}, nil
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
		t.Fatalf("expected repeated prompt handler to answer 2 prompts, got %d", len(prompts))
	}
	for _, prompt := range prompts {
		if prompt.Kind != PromptConfirm || prompt.Prompt != "Accept data-loss warning?" {
			t.Fatalf("unexpected prompt: %+v", prompt)
		}
	}
	found := false
	for _, line := range lines {
		if strings.Contains(line, "confirmed twice") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected confirmation output, got %v", lines)
	}
}

func TestRunTruncatesLogPerAttempt(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "task.log")
	testcmd := testutil.BuildTestCommand(t)
	if _, err := Run(context.Background(), CommandSpec{
		Name:    testcmd,
		Args:    []string{"emit", "first"},
		LogPath: logPath,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), CommandSpec{
		Name:    testcmd,
		Args:    []string{"emit", "second"},
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

func TestRunAppendLogKeepsExistingAttemptLines(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "task.log")
	if err := os.WriteFile(logPath, []byte("stdout: before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testcmd := testutil.BuildTestCommand(t)
	if _, err := Run(context.Background(), CommandSpec{
		Name:      testcmd,
		Args:      []string{"emit", "after"},
		LogPath:   logPath,
		AppendLog: true,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(data))
	if got != "stdout: before\nstdout: after" {
		t.Fatalf("expected appended log, got %q", got)
	}
}

func TestStartWaitIsCleanAfterIntentionalStop(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "service.log")
	testcmd := testutil.BuildTestCommand(t)
	handle, err := Start(context.Background(), CommandSpec{
		Name:    testcmd,
		Args:    []string{"serve"},
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
	bin := filepath.Join(t.TempDir(), "promptcli"+exeSuffix())
	cmd := exec.Command("go", "build", "-o", bin, "./internal/testutil/promptcli")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build prompt cli: %v\n%s", err, string(out))
	}
	return bin
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
