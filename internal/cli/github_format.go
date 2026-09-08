package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benjaco/devflow/internal/logstream"
	"github.com/benjaco/devflow/pkg/api"
)

const (
	githubSummaryMaxRows      = 200
	githubSummaryCellBytes    = 160
	githubStepSummaryMaxBytes = 1024 * 1024
)

func githubWriteJSON(out io.Writer, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	// Legacy runner commands are recognized inside quoted JSON too. Escaping
	// their bracket preserves decoded evidence without adding runner commands.
	for {
		index := bytes.Index(data, []byte("##["))
		if index < 0 {
			break
		}
		if _, err = out.Write(data[:index+2]); err != nil {
			return err
		}
		if _, err = io.WriteString(out, "\\u005b"); err != nil {
			return err
		}
		data = data[index+3:]
	}
	_, err = out.Write(append(data, '\n'))
	return err
}

func githubWriteRunText(out io.Writer, result *api.RunResult, view *api.ExecutionView) error {
	var output bytes.Buffer
	if view != nil {
		// Compact metadata is already bounded. Excerpts have appeared in their
		// attempt groups and must not become a second historical command stream.
		copy := *view
		copy.FailureExcerpts = nil
		if err := writeExecutionView(&output, &copy); err != nil {
			return err
		}
		if len(view.FailureExcerpts) > 0 {
			output.WriteString("Failure excerpts remain available in retained task logs.\n")
		}
	} else if err := writeRunText(&output, result); err != nil {
		return err
	}
	_, err := githubLogReplacer.WriteString(out, output.String())
	return err
}

func githubTaskStatus(state api.NodeState) string {
	if state == api.StateDone {
		return "SUCCESS"
	}
	if state == "" {
		return "UNKNOWN"
	}
	return strings.ToUpper(string(state))
}

func githubAttemptDuration(attempt api.TaskAttempt) string {
	if attempt.StartedAt.IsZero() || attempt.FinishedAt.IsZero() || attempt.FinishedAt.Before(attempt.StartedAt) {
		return "duration unavailable"
	}
	return max(time.Millisecond, attempt.FinishedAt.Sub(attempt.StartedAt).Round(time.Millisecond)).String()
}

// The caller owns serialization: replay never runs in an engine event callback.
func githubWriteAttemptGroup(ctx context.Context, out io.Writer, attempt api.TaskAttempt) (resultErr error) {
	title := fmt.Sprintf("%s | %s | %s", githubBoundedText(attempt.Task, 512), githubTaskStatus(attempt.State), githubAttemptDuration(attempt))
	if !attempt.LogsComplete {
		title += " | output incomplete"
	}
	if _, err := fmt.Fprintf(out, "::group::%s\n", githubCommandData(title)); err != nil {
		return err
	}
	// A missing/corrupt log must not consume the rest of the step into this group.
	defer func() {
		_, err := io.WriteString(out, "::endgroup::\n")
		resultErr = errors.Join(resultErr, err)
	}()
	if !attempt.LogsComplete {
		if _, err := io.WriteString(out, "[devflow] Output is incomplete; showing the retained snapshot available after cleanup.\n"); err != nil {
			return err
		}
	}
	emitted := false
	err := logstream.Stream(ctx, attempt.LogPath, 0, false, func(line string) error {
		emitted = true
		_, err := fmt.Fprintln(out, githubSafeLogText(line))
		return err
	})
	if err != nil {
		return err
	}
	if !emitted {
		message := "No retained task output."
		if attempt.State == api.StateCached {
			message += " Existing results were reused."
		}
		_, err = fmt.Fprintln(out, message)
	}
	return err
}

func githubWriteFailureAnnotation(out io.Writer, attempt api.TaskAttempt) error {
	if attempt.State != api.StateFailed {
		return nil
	}
	message := githubBoundedText(attempt.LastError, 2048)
	if message == "" {
		message = "Task failed"
	}
	_, err := fmt.Fprintf(out, "::error title=%s::%s; see retained task logs.\n", githubCommandProperty(githubBoundedText(attempt.Task, 512)), githubCommandData(message))
	return err
}

var githubLogReplacer = strings.NewReplacer(
	"::group::", "[child group] ",
	"::endgroup::", "[child endgroup]",
	"##[group]", "[child group] ",
	"##[endgroup]", "[child endgroup]",
	"::", ": :",
	"##[", "# #[",
)

func githubSafeLogText(text string) string {
	// The runner recognizes commands inside a line too. Changing delimiters,
	// including its old syntax, keeps retained output inert without parsing it.
	text = githubLogReplacer.Replace(text)
	return strings.Map(func(r rune) rune {
		if r < ' ' && r != '\t' || r == 0x7f {
			return '\uFFFD'
		}
		return r
	}, strings.NewReplacer("\r", "\\r", "\n", "\\n").Replace(text))
}

func githubCommandData(text string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(text)
}

func githubCommandProperty(text string) string {
	return strings.NewReplacer(":", "%3A", ",", "%2C").Replace(githubCommandData(text))
}

func githubBoundedText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "… [truncated]"
}

func githubMarkdownCell(text string) string {
	text = githubBoundedText(text, githubSummaryCellBytes)
	text = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\t", " ").Replace(text)
	text = html.EscapeString(text)
	// Entities render literal Markdown punctuation without creating table cells,
	// code spans, links, emphasis, HTML, or mentions from adapter-controlled names.
	return strings.NewReplacer("|", "&#124;", "`", "&#96;", "[", "&#91;", "]", "&#93;", "(", "&#40;", ")", "&#41;", "*", "&#42;", "_", "&#95;", "\\", "&#92;", "@", "&#64;", "::", "&#58;&#58;").Replace(text)
}

func githubWriteSummary(out io.Writer, record api.RunRecord, result api.RunResult) error {
	// Keep only displayed identities. Huge graphs retain exact counts without a
	// second map or table proportional to the full graph or its log volume.
	shown := min(len(result.Nodes), githubSummaryMaxRows)
	attempts := make(map[string]api.TaskAttempt, shown)
	for _, node := range result.Nodes[:shown] {
		if node.AttemptID != "" {
			attempts[node.AttemptID] = api.TaskAttempt{}
		}
	}
	for _, attempt := range record.Attempts {
		if _, ok := attempts[attempt.AttemptID]; ok {
			attempts[attempt.AttemptID] = attempt
		}
	}
	counts := map[string]int{}
	for _, node := range result.Nodes {
		counts[githubTaskStatus(node.State)]++
	}
	states := make([]string, 0, len(counts))
	for state := range counts {
		states = append(states, state)
	}
	sort.Strings(states)
	overall := "FAILED"
	if result.Success {
		overall = "SUCCESS"
	}
	if record.State == api.RunCanceled {
		overall = "CANCELED"
	}
	var output bytes.Buffer
	fmt.Fprintf(&output, "\n### Devflow: %s\n\nOverall result: **%s**", githubMarkdownCell(result.Target), overall)
	if result.DurationMs > 0 {
		fmt.Fprintf(&output, " (%s)", (time.Duration(result.DurationMs) * time.Millisecond).String())
	}
	fmt.Fprintf(&output, ". %d nodes.\n", len(result.Nodes))
	if result.Error != nil {
		fmt.Fprintf(&output, "\n%s\n", githubMarkdownCell(result.Error.Message))
	}
	if len(states) > 0 {
		output.WriteString("\n")
		for i, state := range states {
			if i > 0 {
				output.WriteString(" · ")
			}
			fmt.Fprintf(&output, "%s: %d", githubMarkdownCell(state), counts[state])
		}
		output.WriteString("\n")
	}
	output.WriteString("\n| Task | Final state | Duration |\n| --- | --- | --- |\n")
	for _, node := range result.Nodes[:shown] {
		duration := "duration unavailable"
		if attempt, ok := attempts[node.AttemptID]; ok && attempt.AttemptID != "" {
			duration = githubAttemptDuration(attempt)
		} else if node.DurationMs > 0 && node.AttemptID != "" {
			duration = (time.Duration(node.DurationMs) * time.Millisecond).String()
		}
		fmt.Fprintf(&output, "| %s | %s | %s |\n", githubMarkdownCell(node.Name), githubMarkdownCell(githubTaskStatus(node.State)), duration)
	}
	if omitted := len(result.Nodes) - shown; omitted > 0 {
		fmt.Fprintf(&output, "\n%d rows omitted; counts above include every node. Full node evidence remains in the final result and retained run.\n", omitted)
	}
	_, err := out.Write(output.Bytes())
	return err
}

func githubAppendSummary(path string, record api.RunRecord, result api.RunResult) (resultErr error) {
	if path == "" {
		return nil
	}
	var summary bytes.Buffer
	if err := githubWriteSummary(&summary, record, result); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("GitHub step summary is not a regular file")
	}
	if info.Size()+int64(summary.Len()) > githubStepSummaryMaxBytes {
		return fmt.Errorf("GitHub step summary would exceed the 1 MiB step limit")
	}
	_, err = f.Write(summary.Bytes())
	return err
}
