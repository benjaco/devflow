package database

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/benjaco/devflow/pkg/process"
)

var payloadTerminalControl = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
var payloadChoiceLine = regexp.MustCompile(`^(?:❯\s*)?([+~]\s+.+?\s+(?:create|rename|move|rename/move) (?:column|table|enum|schema|sequence|role|policy|view))`)
var payloadConfirm = regexp.MustCompile(`(?s)^\?\s+(.+?)\s+[›»]\s+\([yYnN]/[yYnN]\)`)

// Payload 3 uses Hanji menus for Drizzle conflicts and prompts for confirmations.
// Each new question hides the cursor; answer redraws do not. Requiring that
// boundary prevents arrow-key redraws from becoming duplicate public questions.
func parsePayloadPrompt(output string) *process.PromptMatch {
	start := strings.LastIndex(output, "\x1b[?25l")
	if start < 0 || strings.LastIndex(output, "\x1b[?25h") > start {
		return nil
	}
	text := strings.TrimSpace(payloadTerminalControl.ReplaceAllString(output[start:], ""))
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
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "Is ") || !strings.Contains(lines[0], " created or renamed from another ") || !strings.HasSuffix(lines[0], "?") {
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
