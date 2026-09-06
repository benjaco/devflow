package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/benjaco/devflow/internal/clierror"
	"github.com/benjaco/devflow/pkg/api"
)

func addResultFlags(fs *flag.FlagSet) {
	fs.String("details", "full", "result detail: summary, issues, or full")
	fs.String("progress", "logs", "progress verbosity: quiet, states, or logs; independent of final output")
}

// Validate before adapter bootstrap; presentation options must not start work.
func (a *App) configureResultOutput(fs *flag.FlagSet) error {
	if fs == nil || (fs.Name() != "run" && fs.Name() != "status" && fs.Name() != "flush") {
		return nil
	}
	a.details = fs.Lookup("details").Value.String()
	a.progress = fs.Lookup("progress").Value.String()
	if a.details != "summary" && a.details != "issues" && a.details != "full" {
		return clierror.Wrap(fmt.Errorf("invalid --details %q: want summary, issues, or full", a.details), "invalid_arguments", "parsing")
	}
	if a.progress != "quiet" && a.progress != "states" && a.progress != "logs" {
		return clierror.Wrap(fmt.Errorf("invalid --progress %q: want quiet, states, or logs", a.progress), "invalid_arguments", "parsing")
	}
	if fs.Name() == "run" {
		watch := fs.Lookup("watch").Value.String() == "true"
		detach := fs.Lookup("detach").Value.String() == "true"
		if a.compactOutput() && (watch || detach) {
			return clierror.Wrap(fmt.Errorf("--details summary|issues requires a finite run; inspect watch with status or flush"), "invalid_arguments", "parsing")
		}
		if watch && a.progress != "logs" {
			return clierror.Wrap(fmt.Errorf("--progress quiet|states applies to finite run output; watch emits an event stream"), "invalid_arguments", "parsing")
		}
	}
	return nil
}

func (a *App) progressWriter() io.Writer {
	if a.progress == "quiet" {
		return io.Discard
	}
	return a.Stderr
}

func (a *App) compactOutput() bool { return a.details == "summary" || a.details == "issues" }

func (a *App) resultView(value any) any {
	if !a.compactOutput() {
		return value
	}
	view := &api.ExecutionView{Details: a.details, Nodes: []api.NodeStatus{}, Issues: []api.FlushIssue{}, PendingPrompts: []api.Prompt{}}
	var nodes []api.NodeStatus
	var issues []api.FlushIssue
	var prompts []api.Prompt
	var excerpts []api.FailureExcerpt
	switch result := value.(type) {
	case *api.RunResult:
		if result == nil {
			return value
		}
		return a.resultView(*result)
	case api.RunResult:
		view.RunID, view.InstanceID, view.Target, view.Mode = result.RunID, result.InstanceID, result.Target, result.Mode
		view.Success, view.Error, view.ResourceConflict = &result.Success, result.Error, result.ResourceConflict
		view.DurationMs, view.StartedAt, view.FinishedAt = result.DurationMs, result.StartedAt, result.FinishedAt
		view.FailedNode = result.FailedNode
		view.Counts.CacheHits, view.Counts.CacheMisses = len(result.CacheHits), len(result.CacheMisses)
		nodes = result.Nodes
		excerpts = result.FailureExcerpts
		if result.RepositoryChanges != nil {
			copy := *result.RepositoryChanges
			copy.ChangedPaths, copy.IgnoredLineEndingPaths, copy.UnexpectedTrackedPaths = nil, nil, nil
			copy.ChangedPathsTruncated = copy.ChangedPathCount > 0
			copy.IgnoredLineEndingPathsTruncated = copy.IgnoredLineEndingPathCount > 0
			copy.UnexpectedTrackedPathsTruncated = copy.UnexpectedTrackedPathCount > 0
			view.RepositoryChanges = &copy
		}
		if result.Lifecycle != nil {
			for _, issue := range result.Lifecycle.Issues {
				issues = append(issues, api.FlushIssue{Task: issue.Resource, Kind: "lifecycle", Message: issue.Reason})
			}
		}
	case api.StatusResult:
		view.RunID, view.InstanceID, view.Target, view.Mode = result.RunID, result.InstanceID, result.Target, result.Mode
		view.Worktree, view.UpdatedAt, view.Daemon = result.Worktree, result.UpdatedAt, result.Daemon
		nodes, prompts = result.Nodes, result.PendingPrompts
	case api.FlushResult:
		view.RunID, view.InstanceID, view.Target, view.Mode = result.RunID, result.InstanceID, result.Target, result.Mode
		view.Worktree, view.UpdatedAt = result.Worktree, result.UpdatedAt
		view.Success, view.Error, view.ResourceConflict = &result.Success, result.Error, result.ResourceConflict
		view.RequestID, view.Synced, view.Started, view.TimedOut = result.RequestID, &result.Synced, &result.Started, result.TimedOut
		view.DurationMs = result.DurationMs
		view.Counts.Services = len(result.Services)
		for _, service := range result.Services {
			if !service.Alive || !service.Ready {
				view.Counts.UnreadyServices++
			}
		}
		nodes, issues = result.Nodes, result.Issues
	default:
		return value
	}
	view.Counts.Nodes, view.Counts.Issues, view.Counts.PendingPrompts = len(nodes), len(issues), len(prompts)
	view.Counts.NodeStates = map[api.NodeState]int{}
	problemNodes := []api.NodeStatus{}
	for _, node := range nodes {
		view.Counts.NodeStates[node.State]++
		if problemNode(node) {
			problemNodes = append(problemNodes, node)
		}
	}
	view.Counts.ProblemNodes = len(problemNodes)
	sort.SliceStable(problemNodes, func(i, j int) bool {
		if primaryProblem(problemNodes[i]) != primaryProblem(problemNodes[j]) {
			return primaryProblem(problemNodes[i])
		}
		return problemNodes[i].Name < problemNodes[j].Name
	})
	limit, bytes := 5, 8*1024
	if a.details == "issues" {
		limit, bytes = 50, 64*1024
	}
	sample := viewSampler{remaining: bytes, truncated: &view.Truncated.Text}
	// Identifiers remain exact or are omitted; a clipped task/path cannot safely
	// be used with prompts respond or logs. Evidence commands use stable run IDs.
	view.Target = sample.identity(view.Target)
	view.Worktree = sample.identity(view.Worktree)
	if view.Daemon != nil {
		copy := *view.Daemon
		copy.LogPath = sample.identity(copy.LogPath)
		view.Daemon = &copy
	}
	if view.ResourceConflict != nil {
		copy := *view.ResourceConflict
		copy.Worktree = sample.identity(copy.Worktree)
		copy.Target = sample.identity(copy.Target)
		view.ResourceConflict = &copy
	}
	if view.Error != nil {
		copy := *view.Error
		copy.Message = sample.text(copy.Message)
		view.Error = &copy
	}
	if view.RepositoryChanges != nil {
		view.RepositoryChanges.Error = sample.text(view.RepositoryChanges.Error)
	}
	view.FailedNode = sample.identity(view.FailedNode)
	for _, node := range problemNodes {
		if len(view.Nodes) >= limit || sample.remaining == 0 {
			break
		}
		// Never copy timing/component/debug/log arrays into a bounded sample.
		if !sample.identities(node.Name, node.LogPath) {
			continue
		}
		view.Nodes = append(view.Nodes, api.NodeStatus{Name: node.Name, State: node.State, Kind: node.Kind, RunID: node.RunID, AttemptID: node.AttemptID, DurationMs: node.DurationMs, LastError: sample.text(node.LastError), LogPath: node.LogPath})
	}
	view.Truncated.Nodes = len(view.Nodes) < len(problemNodes)
	for _, issue := range issues {
		if len(view.Issues) >= limit || sample.remaining == 0 {
			break
		}
		if !sample.identities(issue.Task, issue.LogPath) {
			continue
		}
		view.Issues = append(view.Issues, api.FlushIssue{Task: issue.Task, Kind: sample.text(issue.Kind), Message: sample.text(issue.Message), LogPath: issue.LogPath})
	}
	view.Truncated.Issues = len(view.Issues) < len(issues)
	for _, prompt := range prompts {
		if len(view.PendingPrompts) >= limit || sample.remaining == 0 {
			break
		}
		if !sample.identities(prompt.Task) {
			continue
		}
		prompt.Message = sample.text(prompt.Message)
		view.PendingPrompts = append(view.PendingPrompts, prompt)
	}
	view.Truncated.PendingPrompts = len(view.PendingPrompts) < len(prompts)
	if len(excerpts) == 0 {
		for _, node := range problemNodes {
			excerpts = append(excerpts, node.FailureExcerpts...)
		}
	}
	view.Counts.FailureExcerpts = len(excerpts)
	if a.details == "issues" {
		for _, excerpt := range excerpts {
			if len(view.FailureExcerpts) >= 5 || sample.remaining == 0 {
				break
			}
			if !sample.identities(excerpt.Node, excerpt.LogPath) {
				continue
			}
			copy := excerpt
			copy.Reason = sample.text(excerpt.Reason)
			copy.Lines = nil
			for _, line := range excerpt.Lines {
				if len(copy.Lines) >= 30 || sample.remaining == 0 {
					break
				}
				copy.Lines = append(copy.Lines, sample.text(line))
			}
			if len(copy.Lines) < len(excerpt.Lines) {
				view.Truncated.FailureExcerpts = true
			}
			copy.EndLine = copy.StartLine + len(copy.Lines) - 1
			if len(copy.Lines) > 0 {
				view.FailureExcerpts = append(view.FailureExcerpts, copy)
			}
		}
	}
	view.Truncated.FailureExcerpts = view.Truncated.FailureExcerpts || len(view.FailureExcerpts) < len(excerpts)
	if view.InstanceID != "" {
		view.Evidence.Status = []string{"devflow", "status", "--instance", view.InstanceID, "--details", "full", "--json"}
		if view.RunID != "" {
			view.Evidence.Run = []string{"devflow", "runs", "show", view.RunID, "--instance", view.InstanceID, "--json"}
			view.Evidence.Prompts = []string{"devflow", "prompts", "list", "--run", view.RunID, "--instance", view.InstanceID, "--json"}
		}
	}
	return view
}

func problemNode(node api.NodeStatus) bool {
	if node.LastError != "" {
		return true
	}
	switch node.State {
	case api.StateFailed, api.StateBlocked, api.StateCanceled, api.StateMigrationNeeded, api.StateDegraded, api.StateDirty:
		return true
	}
	return false
}

func primaryProblem(node api.NodeStatus) bool {
	return node.State != api.StateBlocked && node.State != api.StateCanceled
}

type viewSampler struct {
	remaining int
	truncated *bool
}

func (s *viewSampler) identities(values ...string) bool {
	n := 0
	for _, value := range values {
		n += len(value)
		if len(value) > 2048 || n > s.remaining {
			*s.truncated = true
			return false
		}
	}
	s.remaining -= n
	return true
}

func (s *viewSampler) identity(value string) string {
	if !s.identities(value) {
		return ""
	}
	return value
}

func (s *viewSampler) text(value string) string {
	n := min(len(value), s.remaining, 2048)
	if n == len(value) {
		s.remaining -= n
		return value
	}
	*s.truncated = true
	for n > 0 && !utf8.RuneStart(value[n]) {
		n--
	}
	s.remaining -= n
	return value[:n]
}

func writeExecutionView(out io.Writer, view *api.ExecutionView) error {
	_, err := fmt.Fprintf(out, "target=%s instance=%s run=%s details=%s nodes=%d states=%v\n", view.Target, view.InstanceID, view.RunID, view.Details, view.Counts.Nodes, view.Counts.NodeStates)
	if err != nil {
		return err
	}
	if view.Success != nil {
		if _, err = fmt.Fprintf(out, "success=%t\n", *view.Success); err != nil {
			return err
		}
	}
	if view.Synced != nil {
		if _, err = fmt.Fprintf(out, "synced=%t timed_out=%t\n", *view.Synced, view.TimedOut); err != nil {
			return err
		}
	}
	if view.Error != nil {
		if _, err = fmt.Fprintf(out, "%s: %s\n", view.Error.Code, view.Error.Message); err != nil {
			return err
		}
	}
	for _, node := range view.Nodes {
		if _, err = fmt.Fprintf(out, "%s %s: %s log=%s\n", node.Name, node.State, node.LastError, node.LogPath); err != nil {
			return err
		}
	}
	for _, issue := range view.Issues {
		if _, err = fmt.Fprintf(out, "%s %s: %s\n", issue.Kind, issue.Task, issue.Message); err != nil {
			return err
		}
	}
	for _, excerpt := range view.FailureExcerpts {
		if _, err = fmt.Fprintf(out, "%s %s (line %d):\n", excerpt.Node, excerpt.Reason, excerpt.StartLine); err != nil {
			return err
		}
		for _, line := range excerpt.Lines {
			if _, err = fmt.Fprintln(out, line); err != nil {
				return err
			}
		}
	}
	if view.Counts.PendingPrompts > 0 {
		if _, err = fmt.Fprintf(out, "pending_prompts=%d\n", view.Counts.PendingPrompts); err != nil {
			return err
		}
	}
	if view.Truncated.Nodes || view.Truncated.Issues || view.Truncated.PendingPrompts || view.Truncated.FailureExcerpts || view.Truncated.Text {
		if _, err = fmt.Fprintln(out, "details truncated; retrieve full evidence"); err != nil {
			return err
		}
	}
	command := view.Evidence.Run
	if len(command) == 0 {
		command = view.Evidence.Status
	}
	if len(command) > 0 {
		_, err = fmt.Fprintln(out, strings.Join(command, " "))
	}
	return err
}

func writeRunText(out io.Writer, result *api.RunResult) error {
	if _, err := fmt.Fprintf(out, "target=%s instance=%s success=%v cache_hits=%d", result.Target, result.InstanceID, result.Success, len(result.CacheHits)); err != nil {
		return err
	}
	if change := result.RepositoryChanges; change != nil {
		if _, err := fmt.Fprintf(out, " repository_status=%s commit=%s push_attempted=%t push_succeeded=%t", change.Status, change.CommitSHA, change.PushAttempted, change.PushSucceeded); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(out)
	return err
}
