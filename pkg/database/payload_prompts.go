package database

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/benjaco/devflow/pkg/process"
)

var payloadTerminalControl = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
var payloadTerminalOSC = regexp.MustCompile(`(?s)\x1b\].*?(?:\x07|\x1b\\)`)
var payloadQuestionStart = regexp.MustCompile(`(?m)^(?:Is .+ created or renamed from another .+\?|\? )`)
var payloadChoiceLine = regexp.MustCompile(`^(?:❯\s*)?([+~]\s+.+?\s+(?:create|rename|move|rename/move) (?:column|table|enum|schema|sequence|role|policy|view))`)
var payloadConfirm = regexp.MustCompile(`(?s)^\?\s+(.+?)\s+[›»]\s+\([yYnN]/[yYnN]\)`)

// Payload 3 uses Hanji menus for Drizzle conflicts and prompts for confirmations.
// ConPTY can coalesce show/hide controls between questions and render new rows
// with cursor positioning. Recognize the latest question's text, not a fresh hide.
func parsePayloadPrompt(output string) *process.PromptMatch {
	// ConPTY can insert an OSC title inside a word. Its contents are opaque,
	// including cursor controls; wait if the terminator is in a later read.
	output = payloadTerminalOSC.ReplaceAllString(output, "")
	if strings.Contains(output, "\x1b]") {
		return nil
	}
	if end := strings.LastIndex(output, "\x1b[?25h"); end >= 0 {
		output = output[end+len("\x1b[?25h"):]
	}
	text := payloadTerminalControl.ReplaceAllStringFunc(output, func(control string) string {
		switch control[len(control)-1] {
		case 'H', 'f', 'E':
			return "\n" // Positioned rows separate question/choice text.
		case 'C':
			return " " // ConPTY may encode label padding as cursor movement.
		default:
			return ""
		}
	})
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	starts := payloadQuestionStart.FindAllStringIndex(text, -1)
	if len(starts) == 0 {
		return nil
	}
	text = strings.TrimSpace(text[starts[len(starts)-1][0]:])
	if match := payloadConfirm.FindStringSubmatch(text); match != nil {
		return &process.PromptMatch{
			Request: process.PromptRequest{Kind: process.PromptConfirm, Prompt: match[1]},
			Input: func(response process.PromptResponse) (string, error) {
				if response.Value != "y" {
					// Payload often exits zero after a decline. Stop here so an
					// output-convergence retry cannot ask the same question again.
					return "", fmt.Errorf("PayloadCMS prompt declined")
				}
				// prompts submits on y itself; Return could answer the next prompt.
				return "y", nil
			},
		}
	}
	lines := strings.Split(text, "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "Is ") || !strings.Contains(lines[0], " created or renamed from another ") || !strings.HasSuffix(lines[0], "?") {
		return nil
	}
	// Every new Hanji menu starts on create. Our input sends only Down and Return,
	// so a redraw with a later selection is still the question we already answered.
	if !strings.HasPrefix(strings.TrimSpace(lines[1]), "❯") {
		return nil
	}
	var choices []string
	for _, line := range lines[1:] {
		match := payloadChoiceLine.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			break // Async application/logger output can follow the unterminated menu.
		}
		choices = append(choices, strings.Join(strings.Fields(match[1]), " "))
	}
	if len(choices) < 2 {
		return nil
	}
	return &process.PromptMatch{
		Request: process.PromptRequest{Kind: process.PromptSelect, Prompt: lines[0], Choices: choices},
		Input: func(response process.PromptResponse) (string, error) {
			index, err := strconv.Atoi(response.Value)
			if err != nil || index < 0 || index >= len(choices) {
				return "", fmt.Errorf("PayloadCMS selection must be an available choice index")
			}
			return strings.Repeat("\x1b[B", index) + "\r", nil
		},
	}
}

func (p *PayloadCMSComponent) configureInteraction(spec *process.CommandSpec) {
	spec.Terminal = true
	spec.ParsePrompt = parsePayloadPrompt
	spec.Prompts = append(append([]process.PromptSpec(nil), spec.Prompts...), p.prompts...)
}
