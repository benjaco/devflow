package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/term"
)

func TestTerminalRetainsOutputAndExitStatus(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"output", "failure", "secret"} {
		t.Run(mode, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "terminal.log")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := Run(ctx, CommandSpec{
				Name: executable, Args: []string{"-test.run=^TestTerminalProcessHelper$"},
				Env:      map[string]string{"DEVFLOW_TERMINAL_HELPER": mode},
				Terminal: true, LogPath: logPath,
				Prompts: []PromptSpec{{Pattern: "Secret: ", Kind: PromptText, Secret: true}},
				OnPrompt: func(PromptRequest) (PromptResponse, error) {
					return PromptResponse{Value: "fixture-secret-value"}, nil
				},
			})
			if mode == "failure" {
				if err == nil || result.ExitCode != 7 {
					t.Fatalf("exit status lost: %+v %v", result, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if mode == "secret" {
				if strings.Contains(text, "fixture-secret-value") || !strings.Contains(text, "[output hidden after secret response]") {
					t.Fatalf("terminal echo exposed a secret: %q", text)
				}
				return
			}
			for i := range 2000 {
				line := fmt.Sprintf("retained-line-%04d", i)
				if strings.Count(text, line) != 1 {
					t.Fatalf("terminal output lost or duplicated %s", line)
				}
			}
			if !strings.Contains(text, "stderr diagnostic") || !strings.Contains(text, "final partial line") {
				t.Fatal("terminal completion lost its final output")
			}
		})
	}
}

func TestTerminalCanceledBeforeStartDoesNotCreateLog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logPath := filepath.Join(t.TempDir(), "unstarted.log")
	_, err := Run(ctx, CommandSpec{Name: "must-not-start", Terminal: true, LogPath: logPath})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled command returned %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("canceled command touched its log: %v", err)
	}
}

func TestTerminalCancellationStopsDescendants(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle, err := Start(ctx, CommandSpec{
		Name: executable, Args: []string{"-test.run=^TestTerminalProcessHelper$"}, Terminal: true,
		Env: map[string]string{"DEVFLOW_TERMINAL_HELPER": "parent", "DEVFLOW_TERMINAL_READY": ready},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Stop()
	done := make(chan error, 1)
	go func() { done <- handle.Wait() }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("child exited before readiness: %v", err)
		case <-deadline.C:
			t.Fatal("descendant did not start")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-deadline.C:
		t.Fatal("descendant retained terminal output after cancellation")
	}
}

func TestTerminalProcessHelper(t *testing.T) {
	mode := os.Getenv("DEVFLOW_TERMINAL_HELPER")
	if mode == "" {
		return
	}
	if mode == "child" {
		if err := os.WriteFile(os.Getenv("DEVFLOW_TERMINAL_READY"), []byte("ready"), 0o600); err != nil {
			os.Exit(8)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "stdin is not a terminal")
		os.Exit(9)
	}
	if mode == "parent" {
		child := exec.Command(os.Args[0], "-test.run=^TestTerminalProcessHelper$")
		child.Env = append(os.Environ(), "DEVFLOW_TERMINAL_HELPER=child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Run(); err != nil {
			os.Exit(10)
		}
		os.Exit(0)
	}
	if mode == "secret" {
		fmt.Print("Secret: ")
		value, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Print(value)
		os.Exit(0)
	}
	for i := range 2000 {
		fmt.Printf("retained-line-%04d\n", i)
	}
	fmt.Fprintln(os.Stderr, "stderr diagnostic")
	fmt.Print("\nfinal partial line")
	if mode == "failure" {
		os.Exit(7)
	}
	os.Exit(0)
}
