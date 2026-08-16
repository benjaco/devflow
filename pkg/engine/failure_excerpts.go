package engine

import (
	"bufio"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/benjaco/devflow/pkg/api"
)

const (
	failureExcerptMaxWindows    = 5
	failureExcerptMaxLines      = 200
	failureExcerptMaxBytes      = 64 * 1024
	failureExcerptMaxLineBytes  = 8 * 1024
	failureExcerptContextBefore = 5
	failureExcerptContextAfter  = 30
	failureExcerptMaxWindow     = 80
)

var (
	goCompilerLocationPattern = regexp.MustCompile(`(?i)\.go:[0-9]+:[0-9]+:`)
	goTestDiagnosticPattern   = regexp.MustCompile(`(?i)_test\.go:[0-9]+:($|[^0-9])`)
)

type diagnosticLine struct {
	number int
	text   string
}

type pendingFailureExcerpt struct {
	excerpt         api.FailureExcerpt
	wantedEnd       int
	triggerLine     int
	triggerIncluded bool
}

func boundedFailureExcerpts(path, node string) []api.FailureExcerpt {
	result := make([]api.FailureExcerpt, 0)
	if path == "" || node == "" {
		return result
	}
	file, err := os.Open(path)
	if err != nil {
		return result
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64*1024)
	previous := make([]diagnosticLine, 0, failureExcerptContextBefore)
	var active *pendingFailureExcerpt
	totalLines := 0
	totalBytes := 0
	lineNumber := 0

	appendLine := func(target *pendingFailureExcerpt, line diagnosticLine) bool {
		if totalLines >= failureExcerptMaxLines || totalBytes >= failureExcerptMaxBytes {
			return false
		}
		text := line.text
		remaining := failureExcerptMaxBytes - totalBytes
		if len(text)+1 > remaining {
			text = truncateDiagnosticText(text, remaining-1)
		}
		if text == "" && line.text != "" {
			return false
		}
		target.excerpt.Lines = append(target.excerpt.Lines, text)
		target.excerpt.EndLine = line.number
		if line.number == target.triggerLine {
			target.triggerIncluded = true
		}
		totalLines++
		totalBytes += len(text) + 1
		return true
	}

	finishActive := func() {
		if active == nil || len(active.excerpt.Lines) == 0 || !active.triggerIncluded {
			active = nil
			return
		}
		result = append(result, active.excerpt)
		active = nil
	}

	for {
		line, readErr := readBoundedDiagnosticLine(reader, failureExcerptMaxLineBytes)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			break
		}
		if errors.Is(readErr, io.EOF) && line == "" {
			break
		}
		lineNumber++
		current := diagnosticLine{number: lineNumber, text: line}
		reason := classifyFailureLine(line)

		if active != nil && lineNumber > active.wantedEnd {
			finishActive()
		}
		if active == nil && reason != "" && len(result) < failureExcerptMaxWindows && totalLines < failureExcerptMaxLines && totalBytes < failureExcerptMaxBytes {
			priorStart := 0
			if len(result) > 0 {
				previousEnd := result[len(result)-1].EndLine
				for priorStart < len(previous) && previous[priorStart].number <= previousEnd {
					priorStart++
				}
			}
			start := lineNumber
			if priorStart < len(previous) {
				start = previous[priorStart].number
			}
			active = &pendingFailureExcerpt{
				excerpt: api.FailureExcerpt{
					Node:      node,
					LogPath:   path,
					Reason:    reason,
					StartLine: start,
					EndLine:   start - 1,
					Lines:     make([]string, 0, failureExcerptContextBefore+failureExcerptContextAfter+1),
				},
				wantedEnd:   lineNumber + failureExcerptContextAfter,
				triggerLine: lineNumber,
			}
			for _, prior := range previous[priorStart:] {
				if !appendLine(active, prior) {
					break
				}
			}
		}
		if active != nil && lineNumber <= active.wantedEnd {
			if reason != "" {
				if failureReasonPriority(reason) > failureReasonPriority(active.excerpt.Reason) {
					active.excerpt.Reason = reason
				}
				maximumEnd := active.excerpt.StartLine + failureExcerptMaxWindow - 1
				if extended := lineNumber + failureExcerptContextAfter; extended > active.wantedEnd {
					active.wantedEnd = min(extended, maximumEnd)
				}
			}
			if active.excerpt.EndLine < lineNumber && !appendLine(active, current) {
				finishActive()
				break
			}
		}

		previous = append(previous, current)
		if len(previous) > failureExcerptContextBefore {
			previous = previous[len(previous)-failureExcerptContextBefore:]
		}
		if len(result) >= failureExcerptMaxWindows && active == nil {
			break
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	finishActive()
	return result
}

func readBoundedDiagnosticLine(reader *bufio.Reader, maxBytes int) (string, error) {
	if reader == nil || maxBytes <= 0 {
		return "", io.EOF
	}
	const suffix = "… [line truncated]"
	kept := make([]byte, 0, min(maxBytes, 1024))
	truncated := false
	sawData := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			sawData = true
			content := fragment
			if content[len(content)-1] == '\n' {
				content = content[:len(content)-1]
			}
			if len(kept) < maxBytes {
				count := min(len(content), maxBytes-len(kept))
				kept = append(kept, content[:count]...)
				if count < len(content) {
					truncated = true
				}
			} else if len(content) > 0 {
				truncated = true
			}
		}
		switch {
		case err == nil:
			return finishDiagnosticLine(kept, truncated, maxBytes, suffix), nil
		case errors.Is(err, bufio.ErrBufferFull):
			truncated = truncated || len(kept) >= maxBytes
			continue
		case errors.Is(err, io.EOF):
			if !sawData {
				return "", io.EOF
			}
			return finishDiagnosticLine(kept, truncated, maxBytes, suffix), io.EOF
		default:
			return finishDiagnosticLine(kept, truncated, maxBytes, suffix), err
		}
	}
}

func finishDiagnosticLine(value []byte, truncated bool, maxBytes int, suffix string) string {
	text := strings.TrimSuffix(string(value), "\r")
	if !truncated {
		return text
	}
	return truncateDiagnosticText(text, maxBytes-len(suffix)) + suffix
}

func truncateDiagnosticText(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for value != "" && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func classifyFailureLine(line string) string {
	line = strings.TrimSpace(stripTaskLogPrefix(line))
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(line, "--- FAIL:"):
		return "go-test-failure"
	case strings.HasPrefix(lower, "panic:") || strings.Contains(lower, "fatal error:"):
		return "panic"
	case goCompilerLocationPattern.MatchString(line) || strings.Contains(lower, "undefined:") || strings.Contains(lower, "cannot use ") || strings.Contains(lower, "syntax error:"):
		return "compiler-error"
	case goTestDiagnosticPattern.MatchString(line):
		return "go-test-failure"
	case strings.Contains(line, "AssertionError"):
		return "assertion-error"
	case strings.Contains(lower, "build failed"):
		return "error"
	case strings.HasPrefix(line, "ERROR") || strings.HasPrefix(line, "Error:"):
		return "error"
	case line == "FAIL" || strings.HasPrefix(line, "FAIL\t") || strings.HasPrefix(lower, "failed tests") || strings.HasPrefix(lower, "tests failed") || strings.Contains(lower, " test(s) failed"):
		return "failed-test-summary"
	case strings.Contains(lower, "exited with code") || strings.HasPrefix(lower, "exit status ") || strings.Contains(lower, "process failed"):
		return "process-failure"
	default:
		return ""
	}
}

func stripTaskLogPrefix(line string) string {
	for _, prefix := range []string{"stdout: ", "stderr: "} {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return line
}

func failureReasonPriority(reason string) int {
	switch reason {
	case "go-test-failure":
		return 6
	case "panic":
		return 5
	case "assertion-error":
		return 4
	case "compiler-error":
		return 3
	case "error":
		return 2
	case "failed-test-summary", "process-failure":
		return 1
	default:
		return 0
	}
}
