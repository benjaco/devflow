package reporepair

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/benjaco/devflow/pkg/api"
)

const (
	maxGitStdoutBytes    = 64 << 20
	maxGitStderrBytes    = 64 << 10
	maxReportedPaths     = 200
	maxReportedPathBytes = 64 << 10
	maxReportedPathLen   = 4 << 10
	maxErrorDetailBytes  = 4 << 10
)

var credentialURLPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@]+(?::[^\s/@]*)?@`)

type Options struct {
	Pathspecs       []string
	Message         string
	Push            bool
	FailAfterCommit bool
}

type Runner struct {
	root         string
	options      Options
	progress     io.Writer
	gitPath      string
	baselineHead string
	baselineRef  string
	preflightOK  bool
}

type statusEntry struct {
	index    byte
	worktree byte
	path     string
}

func New(root string, options Options, progress io.Writer) *Runner {
	options.Pathspecs = append([]string(nil), options.Pathspecs...)
	return &Runner{root: root, options: options, progress: progress}
}

func (r *Runner) Preflight(ctx context.Context) (api.RepositoryChangeResult, error) {
	result := r.newResult(api.RepositoryChangeStatusPreconditionFailed)
	r.progressf("repository repair preflight: checking Git worktree and HEAD")

	gitPath, err := exec.LookPath("git")
	if err != nil {
		return r.fail(result, fmt.Errorf("repository repair requires Git on PATH: %w", err))
	}
	r.gitPath = gitPath
	if err := r.requireRepositoryRoot(ctx); err != nil {
		return r.fail(result, err)
	}

	head, err := r.objectID(ctx, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return r.fail(result, fmt.Errorf("repository repair requires an existing HEAD commit: %w", err))
	}
	ref, err := r.gitText(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return r.fail(result, fmt.Errorf("resolve repository HEAD ref: %w", err))
	}

	dirty, err := r.status(ctx, true, nil)
	if err != nil {
		return r.fail(result, fmt.Errorf("inspect repository precondition: %w", err))
	}
	if len(dirty) > 0 {
		r.setChangedPaths(&result, statusPaths(dirty))
		return r.fail(result, fmt.Errorf("repository repair requires a clean Git worktree; found %d changed path(s)", result.ChangedPathCount))
	}
	// Ask Git to parse every pathspec before the DAG starts. This preserves all
	// Git pathspec magic without trying to reproduce it in Devflow.
	if _, err := r.status(ctx, true, r.options.Pathspecs); err != nil {
		return r.fail(result, fmt.Errorf("validate repository repair pathspecs: %w", err))
	}

	r.baselineHead = head
	r.baselineRef = ref
	r.preflightOK = true
	r.progressf("repository repair preflight: clean at %s", shortObjectID(head))
	return result, nil
}

func (r *Runner) SkippedDAGFailure() api.RepositoryChangeResult {
	result := r.newResult(api.RepositoryChangeStatusSkippedDAGFailed)
	result.Error = "repository repair skipped because the DAG failed"
	r.progressf("repository repair: skipped because the DAG failed")
	return result
}

func (r *Runner) Apply(ctx context.Context) (api.RepositoryChangeResult, error) {
	result := r.newResult(api.RepositoryChangeStatusRepositoryStateChanged)
	if !r.preflightOK {
		return r.fail(result, fmt.Errorf("repository repair preflight did not complete"))
	}
	r.progressf("repository repair: inspecting permitted pathspecs")

	currentHead, err := r.objectID(ctx, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return r.fail(result, fmt.Errorf("verify repository HEAD after DAG success: %w", err))
	}
	currentRef, err := r.gitText(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return r.fail(result, fmt.Errorf("verify repository HEAD ref after DAG success: %w", err))
	}
	if currentHead != r.baselineHead || currentRef != r.baselineRef {
		return r.fail(result, fmt.Errorf("repository HEAD changed during DAG execution; refusing to commit or push"))
	}

	permitted, unexpected, err := r.inspectChanges(ctx)
	if err != nil {
		return r.fail(result, err)
	}
	r.setChangedPaths(&result, permitted)
	r.setUnexpectedPaths(&result, unexpected)
	if len(unexpected) > 0 {
		result.Status = api.RepositoryChangeStatusUnexpectedTrackedChanges
		return r.fail(result, fmt.Errorf("DAG changed %d tracked path(s) outside --commit-path", len(unexpected)))
	}
	if len(permitted) == 0 {
		result.Status = api.RepositoryChangeStatusNoChanges
		r.progressf("repository repair: no permitted changes found")
		return result, nil
	}

	r.progressf("repository repair: staging %d permitted path(s)", len(permitted))
	addArgs := []string{"add", "-A", "--"}
	addArgs = append(addArgs, r.options.Pathspecs...)
	if _, err := r.git(ctx, nil, nil, addArgs...); err != nil {
		return r.failAfterStaging(ctx, result, api.RepositoryChangeStatusCommitFailed, fmt.Errorf("stage permitted repository changes: %w", err))
	}

	staged, unexpected, err := r.verifyStagedChanges(ctx)
	if err != nil {
		return r.failAfterStaging(ctx, result, api.RepositoryChangeStatusCommitFailed, err)
	}
	r.setChangedPaths(&result, staged)
	r.setUnexpectedPaths(&result, unexpected)
	if missing := pathDifference(permitted, staged); len(missing) > 0 || len(pathDifference(staged, permitted)) > 0 {
		return r.failAfterStaging(ctx, result, api.RepositoryChangeStatusCommitFailed, fmt.Errorf("permitted repository paths changed while staging; refusing a partial commit"))
	}
	if len(unexpected) > 0 {
		return r.failAfterStaging(ctx, result, api.RepositoryChangeStatusUnexpectedTrackedChanges, fmt.Errorf("repository changed %d tracked path(s) outside --commit-path while staging", len(unexpected)))
	}

	commitEnv, err := r.commitEnvironment(ctx)
	if err != nil {
		return r.failAfterStaging(ctx, result, api.RepositoryChangeStatusCommitFailed, err)
	}
	tree, err := r.objectID(ctx, "write-tree")
	if err != nil {
		return r.failAfterStaging(ctx, result, api.RepositoryChangeStatusCommitFailed, fmt.Errorf("write staged repository tree: %w", err))
	}
	message := r.options.Message
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	commitOut, err := r.git(ctx, strings.NewReader(message), commitEnv, "commit-tree", tree, "-p", r.baselineHead)
	if err != nil {
		return r.failAfterStaging(ctx, result, api.RepositoryChangeStatusCommitFailed, fmt.Errorf("create repository repair commit: %w", err))
	}
	commitSHA, err := parseObjectID(commitOut)
	if err != nil {
		return r.failAfterStaging(ctx, result, api.RepositoryChangeStatusCommitFailed, fmt.Errorf("parse repository repair commit: %w", err))
	}
	if _, err := r.git(ctx, nil, nil, "update-ref", "-m", "devflow repository repair", "HEAD", commitSHA, r.baselineHead); err != nil {
		return r.failAfterStaging(ctx, result, api.RepositoryChangeStatusCommitFailed, fmt.Errorf("publish repository repair commit to HEAD: %w", err))
	}
	result.Status = api.RepositoryChangeStatusCommitted
	result.CommitCreated = true
	result.CommitSHA = commitSHA
	r.progressf("repository repair: created commit %s", shortObjectID(commitSHA))

	if r.options.Push {
		result.PushAttempted = true
		r.progressf("repository repair: pushing commit %s", shortObjectID(commitSHA))
		if _, err := r.git(ctx, nil, nil, "push"); err != nil {
			result.Status = api.RepositoryChangeStatusPushFailed
			return r.fail(result, fmt.Errorf("repository repair commit %s was created locally, but git push failed: %w", shortObjectID(commitSHA), err))
		}
		result.PushSucceeded = true
		result.Status = api.RepositoryChangeStatusPushed
		r.progressf("repository repair: push succeeded")
	}

	if r.options.FailAfterCommit {
		result.Status = api.RepositoryChangeStatusFailedAfterCommit
		result.FailAfterCommitTriggered = true
		return r.fail(result, fmt.Errorf("repository repair commit %s was created; failing deliberately because --fail-after-commit was set", shortObjectID(commitSHA)))
	}
	return result, nil
}

func (r *Runner) inspectChanges(ctx context.Context) (permitted []string, unexpected []string, err error) {
	permittedEntries, err := r.status(ctx, true, r.options.Pathspecs)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect permitted repository changes: %w", err)
	}
	allTrackedEntries, err := r.status(ctx, false, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect tracked repository changes: %w", err)
	}
	permittedTrackedEntries, err := r.status(ctx, false, r.options.Pathspecs)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect permitted tracked repository changes: %w", err)
	}
	return statusPaths(permittedEntries), pathDifference(statusPaths(allTrackedEntries), statusPaths(permittedTrackedEntries)), nil
}

func (r *Runner) verifyStagedChanges(ctx context.Context) (staged []string, unexpected []string, err error) {
	permittedStatus, err := r.status(ctx, true, r.options.Pathspecs)
	if err != nil {
		return nil, nil, fmt.Errorf("verify staged permitted changes: %w", err)
	}
	for _, entry := range permittedStatus {
		if entry.index == '?' || entry.worktree != ' ' {
			return nil, nil, fmt.Errorf("permitted path %q changed while Devflow was staging it; refusing a partial commit", truncateUTF8(strings.ToValidUTF8(entry.path, "�"), maxReportedPathLen))
		}
	}
	allTrackedEntries, err := r.status(ctx, false, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("verify tracked repository changes after staging: %w", err)
	}
	permittedTrackedEntries, err := r.status(ctx, false, r.options.Pathspecs)
	if err != nil {
		return nil, nil, fmt.Errorf("verify permitted tracked repository changes after staging: %w", err)
	}
	unexpected = pathDifference(statusPaths(allTrackedEntries), statusPaths(permittedTrackedEntries))

	staged, err = r.diffCachedPaths(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect staged repository changes: %w", err)
	}
	permittedStaged, err := r.diffCachedPaths(ctx, r.options.Pathspecs)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect permitted staged repository changes: %w", err)
	}
	unexpected = uniqueSorted(append(unexpected, pathDifference(staged, permittedStaged)...))
	return staged, unexpected, nil
}

func (r *Runner) status(ctx context.Context, includeUntracked bool, pathspecs []string) ([]statusEntry, error) {
	untracked := "no"
	if includeUntracked {
		untracked = "all"
	}
	args := []string{"status", "--porcelain=v1", "-z", "--no-renames", "--ignore-submodules=none", "--untracked-files=" + untracked}
	if pathspecs != nil {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}
	out, err := r.git(ctx, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseStatus(out)
}

func (r *Runner) diffCachedPaths(ctx context.Context, pathspecs []string) ([]string, error) {
	args := []string{"diff", "--cached", "--name-only", "--no-renames", "-z"}
	if pathspecs != nil {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}
	out, err := r.git(ctx, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseNULPaths(out), nil
}

func (r *Runner) commitEnvironment(ctx context.Context) (map[string]string, error) {
	configuredName, nameOK, err := r.gitConfig(ctx, "user.name")
	if err != nil {
		return nil, fmt.Errorf("read configured Git user name: %w", err)
	}
	configuredEmail, emailOK, err := r.gitConfig(ctx, "user.email")
	if err != nil {
		return nil, fmt.Errorf("read configured Git user email: %w", err)
	}
	configured := nameOK && emailOK && strings.TrimSpace(configuredName) != "" && strings.TrimSpace(configuredEmail) != ""

	headAuthorName, headAuthorEmail, headCommitterName, headCommitterEmail, err := r.headIdentity(ctx)
	if err != nil {
		return nil, err
	}
	defaultAuthorName, defaultAuthorEmail := headAuthorName, headAuthorEmail
	defaultCommitterName, defaultCommitterEmail := headCommitterName, headCommitterEmail
	if configured {
		defaultAuthorName, defaultAuthorEmail = configuredName, configuredEmail
		defaultCommitterName, defaultCommitterEmail = configuredName, configuredEmail
	}
	if name, email, ok := environmentIdentity("GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL"); ok {
		defaultAuthorName, defaultAuthorEmail = name, email
	}
	if name, email, ok := environmentIdentity("GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"); ok {
		defaultCommitterName, defaultCommitterEmail = name, email
	}
	return map[string]string{
		"GIT_AUTHOR_NAME":     defaultAuthorName,
		"GIT_AUTHOR_EMAIL":    defaultAuthorEmail,
		"GIT_COMMITTER_NAME":  defaultCommitterName,
		"GIT_COMMITTER_EMAIL": defaultCommitterEmail,
	}, nil
}

func (r *Runner) headIdentity(ctx context.Context) (string, string, string, string, error) {
	out, err := r.git(ctx, nil, nil, "show", "-s", "--format=%an%x00%ae%x00%cn%x00%ce", r.baselineHead)
	if err != nil {
		return "", "", "", "", fmt.Errorf("derive Git commit identity from HEAD: %w", err)
	}
	out = bytes.TrimSuffix(out, []byte{'\n'})
	out = bytes.TrimSuffix(out, []byte{'\r'})
	parts := bytes.Split(out, []byte{0})
	if len(parts) != 4 {
		return "", "", "", "", fmt.Errorf("derive Git commit identity from HEAD: expected four identity fields")
	}
	values := []string{string(parts[0]), string(parts[1]), string(parts[2]), string(parts[3])}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return "", "", "", "", fmt.Errorf("derive Git commit identity from HEAD: identity field is empty")
		}
	}
	return values[0], values[1], values[2], values[3], nil
}

func (r *Runner) gitConfig(ctx context.Context, key string) (string, bool, error) {
	out, err := r.git(ctx, nil, nil, "config", "--get", key)
	if err == nil {
		return trimCommandText(out), true, nil
	}
	var exitErr *gitCommandError
	if !errors.As(err, &exitErr) {
		return "", false, err
	}
	if exitErr.exitCode == 1 {
		return "", false, nil
	}
	return "", false, err
}

func (r *Runner) requireRepositoryRoot(ctx context.Context) error {
	inside, err := r.gitText(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil || inside != "true" {
		if err != nil {
			return fmt.Errorf("repository repair requires a Git worktree: %w", err)
		}
		return fmt.Errorf("repository repair requires a Git worktree")
	}
	top, err := r.gitText(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("resolve Git worktree root: %w", err)
	}
	rootInfo, rootErr := os.Stat(r.root)
	topInfo, topErr := os.Stat(filepath.Clean(top))
	if rootErr != nil || topErr != nil || !os.SameFile(rootInfo, topInfo) {
		return fmt.Errorf("repository repair requires the Devflow worktree to be the Git worktree root (Git root is %s)", top)
	}
	return nil
}

func (r *Runner) gitText(ctx context.Context, args ...string) (string, error) {
	out, err := r.git(ctx, nil, nil, args...)
	if err != nil {
		return "", err
	}
	return trimCommandText(out), nil
}

func (r *Runner) objectID(ctx context.Context, args ...string) (string, error) {
	out, err := r.git(ctx, nil, nil, args...)
	if err != nil {
		return "", err
	}
	return parseObjectID(out)
}

type gitCommandError struct {
	operation string
	exitCode  int
	cause     error
	detail    string
}

func (e *gitCommandError) Error() string {
	if e.detail == "" {
		return fmt.Sprintf("git %s failed: %v", e.operation, e.cause)
	}
	return fmt.Sprintf("git %s failed: %v: %s", e.operation, e.cause, e.detail)
}

func (e *gitCommandError) Unwrap() error { return e.cause }

func (r *Runner) git(ctx context.Context, stdin io.Reader, overrides map[string]string, args ...string) ([]byte, error) {
	if r.gitPath == "" {
		return nil, fmt.Errorf("git executable was not resolved")
	}
	stdout := &boundedBuffer{max: maxGitStdoutBytes}
	stderr := &boundedBuffer{max: maxGitStderrBytes}
	cmd := exec.CommandContext(ctx, r.gitPath, args...)
	cmd.Dir = r.root
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	baseOverrides := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GCM_INTERACTIVE":     "Never",
	}
	for key, value := range overrides {
		baseOverrides[key] = value
	}
	cmd.Env = mergedEnv(os.Environ(), baseOverrides)
	err := cmd.Run()
	operation := "command"
	if len(args) > 0 {
		operation = args[0]
	}
	if stdout.truncated {
		return nil, fmt.Errorf("git %s output exceeded the %d-byte safety limit", operation, maxGitStdoutBytes)
	}
	if err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		detail := boundedErrorDetail(stderr.String())
		return nil, &gitCommandError{operation: operation, exitCode: exitCode, cause: err, detail: detail}
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func (r *Runner) failAfterStaging(ctx context.Context, result api.RepositoryChangeResult, status api.RepositoryChangeStatus, cause error) (api.RepositoryChangeResult, error) {
	result.Status = status
	if _, resetErr := r.git(ctx, nil, nil, "reset", "--mixed", "--quiet", "HEAD"); resetErr != nil {
		cause = fmt.Errorf("%w; additionally failed to restore the clean index: %v", cause, resetErr)
	}
	return r.fail(result, cause)
}

func (r *Runner) fail(result api.RepositoryChangeResult, err error) (api.RepositoryChangeResult, error) {
	result.Error = err.Error()
	r.progressf("repository repair: %s", result.Error)
	return result, err
}

func (r *Runner) newResult(status api.RepositoryChangeStatus) api.RepositoryChangeResult {
	return api.RepositoryChangeResult{
		Status:                   status,
		ChangedPaths:             []string{},
		UnexpectedTrackedPaths:   []string{},
		FailAfterCommitRequested: r.options.FailAfterCommit,
	}
}

func (r *Runner) setChangedPaths(result *api.RepositoryChangeResult, paths []string) {
	result.ChangedPaths, result.ChangedPathCount, result.ChangedPathsTruncated = reportedPaths(paths)
}

func (r *Runner) setUnexpectedPaths(result *api.RepositoryChangeResult, paths []string) {
	result.UnexpectedTrackedPaths, result.UnexpectedTrackedPathCount, result.UnexpectedTrackedPathsTruncated = reportedPaths(paths)
}

func (r *Runner) progressf(format string, args ...any) {
	if r.progress == nil {
		return
	}
	_, _ = fmt.Fprintf(r.progress, "[devflow] "+format+"\n", args...)
}

func parseStatus(data []byte) ([]statusEntry, error) {
	records := bytes.Split(data, []byte{0})
	entries := make([]statusEntry, 0, len(records))
	for _, record := range records {
		if len(record) == 0 {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return nil, fmt.Errorf("parse Git status: malformed porcelain-v1 record")
		}
		entries = append(entries, statusEntry{index: record[0], worktree: record[1], path: string(record[3:])})
	}
	return entries, nil
}

func parseNULPaths(data []byte) []string {
	parts := bytes.Split(data, []byte{0})
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			paths = append(paths, string(part))
		}
	}
	return uniqueSorted(paths)
}

func statusPaths(entries []statusEntry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.path)
	}
	return uniqueSorted(paths)
}

func pathDifference(all, permitted []string) []string {
	allowed := make(map[string]struct{}, len(permitted))
	for _, path := range permitted {
		allowed[path] = struct{}{}
	}
	out := make([]string, 0)
	for _, path := range all {
		if _, ok := allowed[path]; !ok {
			out = append(out, path)
		}
	}
	return uniqueSorted(out)
}

func uniqueSorted(paths []string) []string {
	set := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		set[path] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for path := range set {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func reportedPaths(paths []string) ([]string, int, bool) {
	paths = uniqueSorted(paths)
	reported := make([]string, 0, min(len(paths), maxReportedPaths))
	totalBytes := 0
	truncated := false
	for _, path := range paths {
		if len(reported) >= maxReportedPaths {
			truncated = true
			break
		}
		path = strings.ToValidUTF8(path, "�")
		path = truncateUTF8(path, maxReportedPathLen)
		if totalBytes+len(path) > maxReportedPathBytes {
			truncated = true
			break
		}
		reported = append(reported, path)
		totalBytes += len(path)
	}
	if len(reported) < len(paths) {
		truncated = true
	}
	return reported, len(paths), truncated
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	const suffix = "..."
	limit := maxBytes - len(suffix)
	if limit <= 0 {
		return suffix[:maxBytes]
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + suffix
}

func parseObjectID(data []byte) (string, error) {
	value := trimCommandText(data)
	if len(value) < 40 || len(value) > 64 {
		return "", fmt.Errorf("unexpected Git object ID %q", value)
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return "", fmt.Errorf("unexpected Git object ID %q", value)
		}
	}
	return strings.ToLower(value), nil
}

func trimCommandText(data []byte) string {
	return strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
}

func shortObjectID(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func environmentIdentity(nameKey, emailKey string) (string, string, bool) {
	name, nameOK := os.LookupEnv(nameKey)
	email, emailOK := os.LookupEnv(emailKey)
	if !nameOK || !emailOK || strings.TrimSpace(name) == "" || strings.TrimSpace(email) == "" {
		return "", "", false
	}
	return name, email, true
}

func mergedEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if envOverrideContains(overrides, key) {
			continue
		}
		out = append(out, item)
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, key+"="+overrides[key])
	}
	return out
}

func envOverrideContains(overrides map[string]string, key string) bool {
	if runtime.GOOS != "windows" {
		_, ok := overrides[key]
		return ok
	}
	for candidate := range overrides {
		if strings.EqualFold(candidate, key) {
			return true
		}
	}
	return false
}

func boundedErrorDetail(value string) string {
	value = credentialURLPattern.ReplaceAllString(value, "$1[REDACTED]@")
	value = strings.TrimSpace(value)
	return truncateUTF8(strings.ToValidUTF8(value, "�"), maxErrorDetailBytes)
}

type boundedBuffer struct {
	bytes.Buffer
	max       int
	truncated bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	originalLen := len(data)
	remaining := b.max - b.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = b.Buffer.Write(data)
	}
	if originalLen > remaining {
		b.truncated = true
	}
	return originalLen, nil
}
