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
	for _, output := range []string{menu + "\x1b[?25h", "DATA LOSS WARNING", "\x1b[?25l\nIs headline column in posts table created or renamed from another column?\n❯ + headline create column"} {
		if match := parsePayloadPrompt(output); match != nil {
			t.Fatalf("incomplete/completed output became a question: %+v", match)
		}
	}
	redraw := strings.NewReplacer("❯ +", "  +", "  ~ title", "❯ ~ title").Replace(menu)
	for _, output := range []string{redraw, strings.TrimPrefix(redraw, "\x1b[?25l")} {
		if match := parsePayloadPrompt(output); match != nil {
			t.Fatalf("arrow redraw became another question: %+v", match)
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

func TestPayloadPromptRepaintedTerminalOutput(t *testing.T) {
	// ConPTY renders screen changes rather than forwarding application bytes.
	// Model coalesced cursor visibility, positioned rows and space compression;
	// these are synthetic frames, not a capture from the hosted Windows job.
	menu := "Is headline column in posts table created or renamed from another column?\r\n❯ + headline create column\r\n  ~ title › headline rename column\r\n  ~ subtitle › headline rename column"
	want := process.PromptRequest{Kind: process.PromptSelect,
		Prompt:  "Is headline column in posts table created or renamed from another column?",
		Choices: []string{"+ headline create column", "~ title › headline rename column", "~ subtitle › headline rename column"},
	}
	for name, output := range map[string]string{
		"cursor already hidden":            menu,
		"completed menu then new question": menu + "\x1b[?25h\r\n" + menu,
		"positioned rows":                  "\x1b[6;1H" + strings.Split(menu, "\r\n")[0] + "\x1b[7;1H❯ + headline create column\x1b[8;3H~ title › headline rename column\x1b[9;3H~ subtitle › headline rename column",
		"compressed padding":               strings.ReplaceAll(menu, "headline create", "headline\x1b[8Ccreate"),
	} {
		t.Run(name, func(t *testing.T) {
			match := parsePayloadPrompt(output)
			if match == nil || !reflect.DeepEqual(match.Request, want) {
				t.Fatalf("pending menu was not preserved: %+v", match)
			}
		})
	}
	t.Run("confirmation after menu", func(t *testing.T) {
		question := "DATA LOSS WARNING: legacy contains one row.\nAccept warnings and push schema to database?"
		match := parsePayloadPrompt("~ title › headline column will be renamed\r\n\x1b[12;1H? " + question + " › (y/N)")
		if match == nil || match.Request.Kind != process.PromptConfirm || match.Request.Prompt != question {
			t.Fatalf("pending confirmation was not preserved: %+v", match)
		}
	})
}

func TestPayloadLiteralConfirmationWithTerminalSpacing(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	prompts := 0
	spec := process.CommandSpec{
		Name: executable, Args: []string{"-test.run=^TestPayloadTerminalChild$"},
		Env: map[string]string{"DEVFLOW_PAYLOAD_CHILD": "literal"},
		OnPrompt: func(request process.PromptRequest) (process.PromptResponse, error) {
			prompts++
			if request.Kind != process.PromptConfirm {
				t.Errorf("unexpected prompt: %+v", request)
			}
			return process.PromptResponse{Value: "y"}, nil
		},
	}
	PayloadCMS("payload").configureInteraction(&spec)
	if _, err := process.Run(ctx, spec); err != nil || prompts != 1 {
		t.Fatalf("terminal confirmation did not complete once: prompts=%d err=%v", prompts, err)
	}
}

func TestPayloadAuthoringTerminalChoicesAndCancellation(t *testing.T) {
	testPayloadAuthoringTerminalChoicesAndCancellation(t, "raw")
}

func TestPayloadAuthoringRepaintedTerminalChoicesAndCancellation(t *testing.T) {
	testPayloadAuthoringTerminalChoicesAndCancellation(t, "repainted")
}

func testPayloadAuthoringTerminalChoicesAndCancellation(t *testing.T, rendering string) {
	t.Helper()
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
			rt := &project.Runtime{Worktree: root, TaskName: "payload_new_migration", LogPath: filepath.Join(root, "task.log"), Env: map[string]string{"DEVFLOW_PAYLOAD_CHILD": "1", "DEVFLOW_PAYLOAD_RENDERING": rendering, "DEVFLOW_MIGRATION_NAME": "fixture"}}
			defer func() {
				if t.Failed() {
					output, err := os.ReadFile(rt.LogPath)
					t.Logf("prompts=%+v terminal output=%q read error=%v", prompts, output, err)
				}
			}()
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
	if os.Getenv("DEVFLOW_PAYLOAD_CHILD") == "literal" {
		fmt.Print("DATA LOSS WARNING: dropping a field may delete data. Accept warnings and create migration? [y/N]:\x1b[1C")
		answer, _ := reader.ReadByte()
		if answer != 'y' {
			os.Exit(13)
		}
		os.Exit(0)
	}
	var answers []string
	for question := range 2 {
		frame := "\x1b[?25l\nIs headline column in posts table created or renamed from another column?\n❯ + headline create column\n  ~ title › headline rename column\n  ~ subtitle › headline rename column"
		if question > 0 && os.Getenv("DEVFLOW_PAYLOAD_RENDERING") == "repainted" {
			frame = strings.TrimPrefix(frame, "\x1b[?25l")
		}
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
		completion := strings.TrimPrefix(frame, "\x1b[?25l") + "\x1b[?25h\n"
		if os.Getenv("DEVFLOW_PAYLOAD_RENDERING") == "repainted" {
			// Model a paint containing both completion and the next hidden prompt:
			// ConPTY omits the transient show/hide while preserving the text.
			completion = strings.ReplaceAll(completion, "\x1b[?25h", "")
		}
		fmt.Print(completion)
	}
	confirmation := "\x1b[?25l? DATA LOSS WARNING: legacy contains one row.\nAccept warnings and push schema to database? › (y/N)"
	if os.Getenv("DEVFLOW_PAYLOAD_RENDERING") == "repainted" {
		confirmation = strings.TrimPrefix(confirmation, "\x1b[?25l")
	}
	fmt.Print(confirmation)
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
