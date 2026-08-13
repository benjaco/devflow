package project

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/benjaco/devflow/internal/pathspec"
	"github.com/benjaco/devflow/pkg/process"
)

const (
	DefaultCommandOutputMaxAttempts = 5
	DefaultCommandOutputRetryDelay  = 250 * time.Millisecond
)

// CommandOutputTasklet runs a finite command until its required output files
// exist. RequiredFiles accepts worktree-relative paths and Devflow glob
// patterns; every entry must match at least one regular file.
//
// A command failure is returned immediately. A successful command is retried
// only when its required files are still missing. CleanOutputDirs removes the
// explicitly listed OutputHashFiles and OutputDirs in one cleanup phase before
// the first attempt. RequireNewFiles makes every required path or pattern wait
// for a match that did not exist after that optional cleanup.
type CommandOutputTasklet struct {
	Command         process.CommandSpec
	RequiredFiles   []string
	OutputDirs      []string
	OutputHashFiles []string
	CleanOutputDirs bool
	RequireNewFiles bool
	MaxAttempts     int
	RetryDelay      time.Duration
}

// Run executes the tasklet in the supplied project runtime.
func (t CommandOutputTasklet) Run(ctx context.Context, rt *Runtime) error {
	config, err := t.normalized(rt)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if config.cleanOutputDirs {
		for _, dir := range config.outputDirs {
			if err := validateCommandOutputCleanupTarget(rt.Worktree, dir); err != nil {
				return fmt.Errorf("clean command output directory %q: %w", dir, err)
			}
		}
		for _, hashFile := range config.outputHashFiles {
			if err := validateCommandOutputHashFile(rt.Worktree, hashFile); err != nil {
				return fmt.Errorf("clean command output hash file %q: %w", hashFile, err)
			}
		}
		// Remove hashes first so a later directory-removal failure can never
		// leave a stale hash claiming that the remaining output is current.
		for _, hashFile := range config.outputHashFiles {
			if err := os.Remove(filepath.Join(rt.Worktree, filepath.FromSlash(hashFile))); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("clean command output hash file %q: %w", hashFile, err)
			}
		}
		for _, dir := range config.outputDirs {
			if err := os.RemoveAll(filepath.Join(rt.Worktree, filepath.FromSlash(dir))); err != nil {
				return fmt.Errorf("clean command output directory %q: %w", dir, err)
			}
		}
	}

	baseline := map[string]bool{}
	if config.requireNewFiles {
		for _, pattern := range config.requiredFiles {
			matches, err := commandOutputMatches(rt.Worktree, pattern)
			if err != nil {
				return fmt.Errorf("inspect required command output %q: %w", pattern, err)
			}
			for _, match := range matches {
				baseline[match] = true
			}
		}
	}

	for attempt := 1; attempt <= config.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rt.RunCmdSpec(ctx, config.command); err != nil {
			return err
		}

		missing, err := commandOutputsMissing(rt.Worktree, config.requiredFiles, baseline, config.requireNewFiles)
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			return nil
		}
		if attempt < config.maxAttempts {
			rt.EmitLogLine("stderr", fmt.Sprintf(
				"command %q completed successfully but required files are not ready (attempt %d/%d): %s; retrying",
				config.command.Name,
				attempt,
				config.maxAttempts,
				strings.Join(missing, "; "),
			))
		}
		if err := waitCommandOutputRetry(ctx, config.retryDelay); err != nil {
			return err
		}

		// Some tools hand work to a short-lived child and return before the
		// filesystem is settled. Recheck after the delay before running the
		// command again so delayed output does not cause a duplicate attempt.
		missing, err = commandOutputsMissing(rt.Worktree, config.requiredFiles, baseline, config.requireNewFiles)
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			return nil
		}
		if attempt == config.maxAttempts {
			return fmt.Errorf("command %q completed successfully %d times without producing required files: %s", config.command.Name, attempt, strings.Join(missing, "; "))
		}
	}
	return nil
}

type normalizedCommandOutputTasklet struct {
	command         process.CommandSpec
	requiredFiles   []string
	outputDirs      []string
	outputHashFiles []string
	cleanOutputDirs bool
	requireNewFiles bool
	maxAttempts     int
	retryDelay      time.Duration
}

func (t CommandOutputTasklet) normalized(rt *Runtime) (normalizedCommandOutputTasklet, error) {
	if rt == nil || strings.TrimSpace(rt.Worktree) == "" {
		return normalizedCommandOutputTasklet{}, fmt.Errorf("command output tasklet requires a worktree runtime")
	}
	if strings.TrimSpace(t.Command.Name) == "" {
		return normalizedCommandOutputTasklet{}, fmt.Errorf("command output tasklet requires a command")
	}
	if len(t.RequiredFiles) == 0 {
		return normalizedCommandOutputTasklet{}, fmt.Errorf("command output tasklet requires at least one required file")
	}
	if t.MaxAttempts < 0 {
		return normalizedCommandOutputTasklet{}, fmt.Errorf("command output tasklet max attempts cannot be negative")
	}
	if t.RetryDelay < 0 {
		return normalizedCommandOutputTasklet{}, fmt.Errorf("command output tasklet retry delay cannot be negative")
	}

	requiredFiles := make([]string, 0, len(t.RequiredFiles))
	for _, value := range t.RequiredFiles {
		normalized, err := normalizeCommandOutputPath(value, true)
		if err != nil {
			return normalizedCommandOutputTasklet{}, fmt.Errorf("invalid required file %q: %w", value, err)
		}
		if err := validateCommandOutputGlob(normalized); err != nil {
			return normalizedCommandOutputTasklet{}, fmt.Errorf("invalid required file %q: %w", value, err)
		}
		requiredFiles = append(requiredFiles, normalized)
	}

	outputDirs := make([]string, 0, len(t.OutputDirs))
	for _, value := range t.OutputDirs {
		normalized, err := normalizeCommandOutputPath(value, false)
		if err != nil {
			return normalizedCommandOutputTasklet{}, fmt.Errorf("invalid command output directory %q: %w", value, err)
		}
		if pathspec.HasGlob(normalized) {
			return normalizedCommandOutputTasklet{}, fmt.Errorf("invalid command output directory %q: glob patterns are not allowed", value)
		}
		firstSegment := strings.Split(normalized, "/")[0]
		if firstSegment == ".git" || firstSegment == ".devflow" {
			return normalizedCommandOutputTasklet{}, fmt.Errorf("invalid command output directory %q: Devflow and Git state directories cannot be cleaned", value)
		}
		outputDirs = append(outputDirs, normalized)
	}
	if t.CleanOutputDirs && len(outputDirs) == 0 {
		return normalizedCommandOutputTasklet{}, fmt.Errorf("clean command output directories requires at least one output directory")
	}
	if t.CleanOutputDirs {
		for _, dir := range outputDirs {
			if !commandOutputDirContainsRequirement(dir, requiredFiles) {
				return normalizedCommandOutputTasklet{}, fmt.Errorf("command output directory %q contains none of the required files", dir)
			}
		}
	}
	outputHashFiles := make([]string, 0, len(t.OutputHashFiles))
	for _, value := range t.OutputHashFiles {
		normalized, err := normalizeCommandOutputPath(value, false)
		if err != nil {
			return normalizedCommandOutputTasklet{}, fmt.Errorf("invalid command output hash file %q: %w", value, err)
		}
		if pathspec.HasGlob(normalized) {
			return normalizedCommandOutputTasklet{}, fmt.Errorf("invalid command output hash file %q: glob patterns are not allowed", value)
		}
		firstSegment := strings.Split(normalized, "/")[0]
		if firstSegment == ".git" || firstSegment == ".devflow" {
			return normalizedCommandOutputTasklet{}, fmt.Errorf("invalid command output hash file %q: Devflow and Git state files cannot be cleaned", value)
		}
		outputHashFiles = append(outputHashFiles, normalized)
	}
	if len(outputHashFiles) > 0 && !t.CleanOutputDirs {
		return normalizedCommandOutputTasklet{}, fmt.Errorf("command output hash files require output-directory cleanup")
	}

	command := t.Command
	command.Dir = execDir(rt, command.Dir)
	command.Env = mergeEnvMaps(rt.Env, command.Env)
	maxAttempts := t.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = DefaultCommandOutputMaxAttempts
	}
	retryDelay := t.RetryDelay
	if retryDelay == 0 {
		retryDelay = DefaultCommandOutputRetryDelay
	}
	return normalizedCommandOutputTasklet{
		command:         command,
		requiredFiles:   uniqueStrings(requiredFiles),
		outputDirs:      uniqueStrings(outputDirs),
		outputHashFiles: uniqueStrings(outputHashFiles),
		cleanOutputDirs: t.CleanOutputDirs,
		requireNewFiles: t.RequireNewFiles,
		maxAttempts:     maxAttempts,
		retryDelay:      retryDelay,
	}, nil
}

func normalizeCommandOutputPath(value string, allowGlob bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("path is empty")
	}
	portable := strings.ReplaceAll(value, "\\", "/")
	if strings.HasPrefix(portable, "/") || strings.HasPrefix(portable, "//") || hasPortableVolumeName(portable) {
		return "", fmt.Errorf("path must be worktree-relative")
	}
	cleaned := path.Clean(portable)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path must stay inside the worktree")
	}
	if !allowGlob && pathspec.HasGlob(cleaned) {
		return "", fmt.Errorf("glob patterns are not allowed")
	}
	return cleaned, nil
}

func hasPortableVolumeName(value string) bool {
	return len(value) >= 2 && value[1] == ':' && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z'))
}

func validateCommandOutputGlob(pattern string) error {
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "**" {
			continue
		}
		if _, err := path.Match(segment, ""); err != nil {
			return fmt.Errorf("malformed glob pattern: %w", err)
		}
	}
	return nil
}

func commandOutputDirContainsRequirement(dir string, requiredFiles []string) bool {
	prefix := dir + "/"
	for _, required := range requiredFiles {
		if strings.HasPrefix(required, prefix) {
			return true
		}
	}
	return false
}

func validateCommandOutputCleanupTarget(worktree, rel string) error {
	current := filepath.Clean(worktree)
	segments := strings.Split(rel, "/")
	for index, segment := range segments {
		current = filepath.Join(current, filepath.FromSlash(segment))
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path traverses symlink %q", strings.Join(segments[:index+1], "/"))
		}
		if !info.IsDir() {
			if index == len(segments)-1 {
				return fmt.Errorf("cleanup target is not a directory")
			}
			return fmt.Errorf("path traverses non-directory %q", strings.Join(segments[:index+1], "/"))
		}
	}
	return nil
}

func validateCommandOutputHashFile(worktree, rel string) error {
	current := filepath.Clean(worktree)
	segments := strings.Split(rel, "/")
	for index, segment := range segments {
		current = filepath.Join(current, filepath.FromSlash(segment))
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path traverses symlink %q", strings.Join(segments[:index+1], "/"))
		}
		if index < len(segments)-1 {
			if !info.IsDir() {
				return fmt.Errorf("path traverses non-directory %q", strings.Join(segments[:index+1], "/"))
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("hash target is not a regular file")
		}
	}
	return nil
}

func commandOutputsMissing(worktree string, patterns []string, baseline map[string]bool, requireNew bool) ([]string, error) {
	missing := make([]string, 0)
	for _, pattern := range patterns {
		matches, err := commandOutputMatches(worktree, pattern)
		if err != nil {
			return nil, fmt.Errorf("inspect required command output %q: %w", pattern, err)
		}
		if len(matches) == 0 {
			missing = append(missing, fmt.Sprintf("%q matched no regular files", pattern))
			continue
		}
		if !requireNew {
			continue
		}
		foundNew := false
		for _, match := range matches {
			if !baseline[match] {
				foundNew = true
				break
			}
		}
		if !foundNew {
			missing = append(missing, fmt.Sprintf("%q matched no newly created files", pattern))
		}
	}
	return missing, nil
}

func commandOutputMatches(worktree, pattern string) ([]string, error) {
	if !pathspec.HasGlob(pattern) {
		info, err := os.Lstat(filepath.Join(worktree, filepath.FromSlash(pattern)))
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, nil
		}
		return []string{pattern}, nil
	}

	base := commandOutputGlobBase(pattern)
	scanRoot := filepath.Join(worktree, filepath.FromSlash(base))
	matches := make([]string, 0)
	err := filepath.WalkDir(scanRoot, func(fullPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) && fullPath == scanRoot {
				return fs.SkipDir
			}
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(worktree, fullPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if pathspec.MatchGlob(pattern, rel) {
			matches = append(matches, rel)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

func commandOutputGlobBase(pattern string) string {
	segments := strings.Split(pattern, "/")
	base := make([]string, 0, len(segments))
	for _, segment := range segments {
		if pathspec.HasGlob(segment) {
			break
		}
		base = append(base, segment)
	}
	return strings.Join(base, "/")
}

func waitCommandOutputRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
