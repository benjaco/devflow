package database

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
	"golang.org/x/term"
)

func TestPayloadPromptRendering(t *testing.T) {
	menu := "\x1b[?25l\r\nIs \x1b[34mheadline\x1b[39m column in posts table created or renamed from another column?\r\n❯ + headline create column\r\n  ~ title › headline rename column\r\n  ~ subtitle › headline rename column"
	for _, suffix := range []string{"", "(node:123) DeprecationWarning: fixture\n[INFO] Generating UP statements...\n"} {
		match := parsePayloadPrompt(menu + suffix)
		if match == nil || match.Request.Kind != process.PromptSelect || len(match.Request.Choices) != 3 {
			t.Fatalf("missing complete menu: %+v", match)
		}
		input, err := match.Input(process.PromptResponse{Value: "2"})
		if err != nil || input != "\x1b[B\x1b[B\r" {
			t.Fatalf("input=%q err=%v", input, err)
		}
		for _, invalid := range []string{"-1", "3", "yes", "1\n"} {
			if _, err := match.Input(process.PromptResponse{Value: invalid}); err == nil {
				t.Fatalf("accepted invalid choice %q", invalid)
			}
		}
	}
	for _, output := range []string{strings.TrimPrefix(menu, "\x1b[?25l"), menu + "\x1b[?25h", "DATA LOSS WARNING", "\x1b[?25l\nIs headline column in posts table created or renamed from another column?\n❯ + headline create column"} {
		if match := parsePayloadPrompt(output); match != nil {
			t.Fatalf("incomplete/completed output became a question: %+v", match)
		}
	}
	for _, question := range []string{
		"No schema changes detected. Would you like to create a blank migration file?",
		"Warnings detected during schema push:\nColumn legacy has 1 non-null values.\nDATA LOSS WARNING\nAccept warnings and push schema to database?",
		"It looks like you've run Payload in dev mode.\nIf you'd like to run migrations, data loss will occur. Would you like to proceed?",
	} {
		match := parsePayloadPrompt("\x1b[?25l\x1b[2K\x1b[1G? " + question + " › (y/N)")
		if match == nil || match.Request.Prompt != question {
			t.Fatalf("warning context lost: %+v", match)
		}
		if value, err := match.Input(process.PromptResponse{Value: "y"}); value != "y" || err != nil {
			t.Fatalf("confirmation=%q, %v", value, err)
		}
		if _, err := match.Input(process.PromptResponse{Value: "n"}); err == nil {
			t.Fatal("decline must stop rather than retry a zero-exit generator")
		}
	}
}

func TestPayloadAuthoringTerminalChoicesAndCancellation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"rename", "create", "decline", "headless", "cancel", "no handler"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "src/migrations"), 0700); err != nil {
				t.Fatal(err)
			}
			p := project.Define(func(_ context.Context, b *project.Builder) error {
				b.Name("payload-terminal")
				payload := PayloadCMS("payload").Command(executable, "-test.run=^TestPayloadTerminalChild$")
				b.Target("author", payload.NewMigration(b))
				return nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var prompts []process.PromptRequest
			rt := &project.Runtime{Worktree: root, TaskName: "payload_new_migration", LogPath: filepath.Join(root, "task.log"), Env: map[string]string{"DEVFLOW_PAYLOAD_CHILD": "1", "DEVFLOW_MIGRATION_NAME": "fixture"}}
			rt.OnPrompt = func(_ string, request process.PromptRequest) (process.PromptResponse, error) {
				prompts = append(prompts, request)
				if scenario == "headless" {
					return process.PromptResponse{}, errors.New("interaction required")
				}
				if scenario == "cancel" {
					cancel()
					return process.PromptResponse{}, ctx.Err()
				}
				if request.Kind == process.PromptConfirm {
					if scenario == "decline" {
						return process.PromptResponse{Value: "n"}, nil
					}
					return process.PromptResponse{Value: "y"}, nil
				}
				if scenario == "create" {
					return process.PromptResponse{Value: "0"}, nil
				}
				return process.PromptResponse{Value: "1"}, nil
			}
			if scenario == "no handler" {
				rt.OnPrompt = nil
			}
			err := p.Tasks()[0].Run(ctx, rt)
			if scenario == "rename" || scenario == "create" {
				if err != nil {
					t.Fatal(err)
				}
				answers, err := os.ReadFile(filepath.Join(root, "src/migrations/fixture.ts"))
				want := "1,1"
				if scenario == "create" {
					want = "0,0"
				}
				if err != nil || string(answers) != want {
					t.Fatalf("wrong terminal selections: %q, %v", answers, err)
				}
				if len(prompts) != 3 || !reflect.DeepEqual(prompts[0].Choices, []string{"+ headline create column", "~ title › headline rename column", "~ subtitle › headline rename column"}) {
					t.Fatalf("prompts=%+v", prompts)
				}
			} else {
				if err == nil || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("did not stop promptly: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, "src/migrations/fixture.ts")); !os.IsNotExist(err) {
					t.Fatalf("failed interaction wrote a migration: %v", err)
				}
				want := 1
				if scenario == "decline" {
					want = 3
				}
				if scenario == "no handler" {
					want = 0
				}
				if len(prompts) != want {
					t.Fatalf("callback repeated after failure: %d want %d", len(prompts), want)
				}
			}
		})
	}
}

func TestPayloadTerminalChild(t *testing.T) {
	if os.Getenv("DEVFLOW_PAYLOAD_CHILD") == "" {
		return
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		os.Exit(9)
	}
	if _, err := term.MakeRaw(int(os.Stdin.Fd())); err != nil {
		os.Exit(10)
	}
	reader := bufio.NewReader(os.Stdin)
	var answers []string
	for range 2 {
		frame := "\x1b[?25l\nIs headline column in posts table created or renamed from another column?\n❯ + headline create column\n  ~ title › headline rename column\n  ~ subtitle › headline rename column"
		// Tiny writes exercise UTF-8 and ANSI boundaries independently of read sizes.
		for _, part := range []byte(frame) {
			_, _ = os.Stdout.Write([]byte{part})
		}
		index := 0
		for {
			value, err := reader.ReadByte()
			if err != nil {
				os.Exit(11)
			}
			if value == '\r' {
				break
			}
			if value == 27 {
				_, _ = reader.ReadByte()
				key, _ := reader.ReadByte()
				if key == 'B' {
					index++
				}
			}
		}
		answers = append(answers, strconv.Itoa(index))
		// Arrow redraws and completion must not trigger another answer.
		fmt.Print(strings.TrimPrefix(frame, "\x1b[?25l") + "\x1b[?25h\n")
	}
	fmt.Print("\x1b[?25l? DATA LOSS WARNING: legacy contains one row.\nAccept warnings and push schema to database? › (y/N)")
	answer, _ := reader.ReadByte()
	if answer != 'y' {
		os.Exit(0)
	}
	fmt.Print("\x1b[?25h\n")
	if err := os.WriteFile("src/migrations/fixture.ts", []byte(strings.Join(answers, ",")), 0600); err != nil {
		os.Exit(12)
	}
	os.Exit(0)
}
