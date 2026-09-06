package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/benjaco/devflow/internal/clierror"
	"github.com/benjaco/devflow/internal/execution"
	"github.com/benjaco/devflow/pkg/planner"
)

type planResult struct {
	*planner.Result
	Worktree     string        `json:"worktree"`
	ConfigDigest string        `json:"configDigest"`
	Execution    planExecution `json:"execution"`
}

type planExecution struct {
	ExclusiveWorktree bool             `json:"exclusiveWorktree"`
	Owner             *execution.Owner `json:"owner"`
	Admission         string           `json:"admission"`
}

func (a *App) planCmd(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "emit stable JSON output")
	files := fs.String("files", "", "comma-separated changed paths relative to the worktree (required)")
	intent := fs.String("intent", "verify", "planning intent: verify")
	projectName := fs.String("project", "", "registered project adapter name")
	worktree := fs.String("worktree", "", "project worktree path; defaults to the current directory")
	if err := a.parseFlags(fs, args); err != nil {
		return err
	}
	changed := splitCSV(*files)
	if len(changed) == 0 {
		return clierror.Wrap(fmt.Errorf("usage: devflow plan --files a,b [--intent verify]"), "invalid_arguments", "parsing")
	}
	if *intent != "verify" {
		return clierror.Wrap(fmt.Errorf("unsupported planning intent %q; expected verify", *intent), "invalid_arguments", "parsing")
	}
	root, err := resolveWorktree(*worktree)
	if err != nil {
		return clierror.Wrap(err, "invalid_worktree", "resolution")
	}
	p, err := resolvedProject(*projectName, root)
	if err != nil {
		return err
	}
	result, err := planner.Build(p, planner.Request{Files: changed, Intent: *intent})
	if err != nil {
		return err
	}
	for i := range result.Checks {
		result.Checks[i].Command = append(result.Checks[i].Command, "--worktree", root)
		if len(result.Checks[i].ValidationCommand) != 0 {
			result.Checks[i].ValidationCommand = append(result.Checks[i].ValidationCommand, "--worktree", root)
		}
	}
	payload := planResult{Result: result, Worktree: root, Execution: planExecution{ExclusiveWorktree: true, Admission: "required"}}
	if *jsonOut {
		a.result = &payload
	}
	payload.ConfigDigest, err = planConfigDigest(root)
	if err != nil {
		result.Resolved = false
		return err
	}
	if payload.ConfigDigest == "" {
		result.Resolved = false
		result.Issues = append(result.Issues, planner.Issue{Code: "configuration_identity_unknown", Message: "no adapter source marker is available to identify configuration"})
	}
	// This is a diagnostic snapshot only. Absence cannot promise that later
	// execution admission will succeed, and presence does not establish liveness.
	payload.Execution.Owner, err = execution.ReadOwner(root)
	if err != nil {
		result.Resolved = false
		return clierror.Wrap(err, "ownership_read_failed", "planning")
	}
	if *jsonOut {
		return a.writeResult(payload)
	}
	_, _ = fmt.Fprintf(a.Stdout, "advisory verification plan for %s\n", result.Project)
	_, _ = fmt.Fprintf(a.Stdout, "resolved: %t\n", result.Resolved)
	_, _ = fmt.Fprintf(a.Stdout, "files: %s\n", strings.Join(result.Files, ", "))
	for _, check := range result.Checks {
		_, _ = fmt.Fprintf(a.Stdout, "%s %s: %s\n", check.Kind, check.Name, strings.Join(check.Command, " "))
		if len(check.ValidationCommand) != 0 {
			_, _ = fmt.Fprintf(a.Stdout, "adapter validation: %s\n", strings.Join(check.ValidationCommand, " "))
		}
	}
	_, _ = fmt.Fprintf(a.Stdout, "closure: %s\n", strings.Join(result.Closure, ", "))
	_, _ = fmt.Fprintf(a.Stdout, "prerequisites (%s): clis=[%s] env=[%s]\n", result.Prerequisites.Availability, strings.Join(result.Prerequisites.CLIs, ", "), strings.Join(result.Prerequisites.Env, ", "))
	for _, conflict := range result.Conflicts {
		_, _ = fmt.Fprintf(a.Stdout, "conflict: %s tasks=[%s] (%s)\n", conflict.Resource, strings.Join(conflict.Tasks, ", "), conflict.Reason)
	}
	for _, issue := range result.Issues {
		_, _ = fmt.Fprintf(a.Stdout, "%s: %s\n", issue.Code, issue.Message)
	}
	if payload.Execution.Owner != nil {
		_, _ = fmt.Fprintf(a.Stdout, "execution owner recorded: pid=%d target=%s; execution admission is required\n", payload.Execution.Owner.PID, payload.Execution.Owner.Target)
	}
	return nil
}

func planConfigDigest(root string) (digest string, err error) {
	marker := filepath.Join(root, localProjectFile)
	if _, err := os.Lstat(marker); errors.Is(err, os.ErrNotExist) {
		// Registered in-process adapters have no local source identity to inspect.
		return "", nil
	} else if err != nil {
		return "", clierror.Wrap(err, "adapter_source_invalid", "bootstrap")
	}
	sources, err := localProjectSourceFiles(marker)
	if err != nil {
		return "", err
	}
	defer func() { err = clierror.Wrap(err, "adapter_source_invalid", "bootstrap") }()
	hash := sha256.New()
	for _, source := range sources {
		data, err := os.ReadFile(source)
		if err != nil {
			return "", fmt.Errorf("read adapter source %s: %w", filepath.Base(source), err)
		}
		// Length-delimited labels and contents distinguish renames and boundaries.
		_, _ = fmt.Fprintf(hash, "%d:%s%d:", len(filepath.Base(source)), filepath.Base(source), len(data))
		_, _ = hash.Write(data)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
