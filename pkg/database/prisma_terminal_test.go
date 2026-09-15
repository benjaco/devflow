package database

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

func TestPrismaMigrationConfirmationRequiresTerminal(t *testing.T) {
	tools := filepath.Join(t.TempDir(), "tools with spaces")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(tools, "main.go")
	if err := os.WriteFile(source, []byte(`package main
import (
 "bufio"
 "fmt"
 "os"
 "strings"
 "golang.org/x/term"
)
func main() {
 fmt.Println("Warning: this migration removes an enum value.")
 if !term.IsTerminal(int(os.Stdin.Fd())) || os.Getenv("TERM") == "dumb" {
  fmt.Fprintln(os.Stderr, "Prisma Migrate has detected that the environment is non-interactive")
  os.Exit(1)
 }
 fmt.Print("Are you sure you want to create this migration?")
 answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
 if strings.TrimSpace(answer) != "y" {
  fmt.Println("Migration cancelled.")
  os.Exit(130)
 }
 fmt.Println("Created migration after confirmation.")
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(tools, "npx"+databaseTestExeSuffix())
	if output, err := exec.Command("go", "build", "-o", executable, source).CombinedOutput(); err != nil {
		t.Fatalf("build terminal fixture: %v\n%s", err, output)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TERM", "dumb")
	for _, tc := range []struct{ name, answer, wantError string }{
		{"accept", "y", ""},
		{"custom command", "y", ""},
		{"decline", "n", "130"},
		{"headless", "", "interaction required"},
		{"cancel", "", "context canceled"},
		{"no handler", "", "without handler"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			logPath := filepath.Join(root, "migration.log")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var prompts int
			rt := &project.Runtime{
				Worktree: root, TaskName: "create-migration", LogPath: logPath,
				OnPrompt: func(task string, req process.PromptRequest) (process.PromptResponse, error) {
					prompts++
					if task != "create-migration" || req.Kind != process.PromptConfirm {
						t.Errorf("incorrect prompt route: task=%q request=%+v", task, req)
					}
					data, err := os.ReadFile(logPath)
					if err != nil || !strings.Contains(string(data), "Warning: this migration removes an enum value.") {
						t.Errorf("warnings missing before confirmation: %s, %v", data, err)
					}
					if tc.name == "headless" {
						return process.PromptResponse{}, errors.New("interaction required")
					}
					if tc.name == "cancel" {
						cancel()
						return process.PromptResponse{}, ctx.Err()
					}
					return process.PromptResponse{Value: tc.answer}, nil
				},
			}
			wantPrompts := 1
			if tc.name == "no handler" {
				rt.OnPrompt, wantPrompts = nil, 0
			}
			command := process.CommandSpec{}
			if tc.name == "custom command" {
				command.Name = executable
			}
			err := GeneratePrismaMigrationForRuntime(ctx, rt, PrismaMigrationGenerateOptions{
				Name: "remove-value", CreateOnly: true,
				Command: command,
			})
			data, readErr := os.ReadFile(logPath)
			created := strings.Contains(string(data), "Created migration after confirmation.")
			if readErr != nil || prompts != wantPrompts || created != (tc.wantError == "") {
				t.Fatalf("incorrect confirmation outcome: err=%v readErr=%v prompts=%d log=%s", err, readErr, prompts, data)
			}
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("migration error=%v, want %q; log=%s", err, tc.wantError, data)
			}
		})
	}
}
