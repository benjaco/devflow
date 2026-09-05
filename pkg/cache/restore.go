package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/benjaco/devflow/internal/fsutil"
)

func validateComponent(kind, value string) error {
	if value == "." || !filepath.IsLocal(value) || strings.ContainsAny(value, "/\\\x00") {
		return fmt.Errorf("cache %s %q must be a single local path component", kind, value)
	}
	return nil
}

func validateEntryNames(task, key string) error {
	if err := validateComponent("task", task); err != nil {
		return err
	}
	return validateComponent("key", key)
}

func validateOutputPath(rel string) error {
	portable := strings.ReplaceAll(rel, `\`, "/")
	driveQualified := len(portable) >= 2 && portable[1] == ':' && ((portable[0] >= 'A' && portable[0] <= 'Z') || (portable[0] >= 'a' && portable[0] <= 'z'))
	if !filepath.IsLocal(rel) || strings.HasPrefix(portable, "/") || strings.ContainsRune(portable, '\x00') || driveQualified {
		return fmt.Errorf("cache output %q must be worktree-relative", rel)
	}
	for _, part := range strings.Split(portable, "/") {
		if part == ".." {
			return fmt.Errorf("cache output %q must not contain a parent path segment", rel)
		}
	}
	clean := filepath.ToSlash(filepath.Clean(rel))
	if clean == "." || clean == ".devflow" || clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return fmt.Errorf("cache output %q overlaps worktree or runtime metadata", rel)
	}
	return nil
}

func validateOutputs(outputs Outputs) error {
	if len(outputs.Files)+len(outputs.Dirs) == 0 {
		return fmt.Errorf("cache entries must declare at least one output")
	}
	files := make(map[string]bool, len(outputs.Files))
	for _, rel := range outputs.Files {
		if err := validateOutputPath(rel); err != nil {
			return err
		}
		files[filepath.Clean(rel)] = true
	}
	for _, rel := range outputs.Dirs {
		if err := validateOutputPath(rel); err != nil {
			return err
		}
		if files[filepath.Clean(rel)] {
			return fmt.Errorf("cache output %q is declared as both a file and directory", rel)
		}
	}
	return nil
}

// validateParents permits a symlink at the explicitly supplied root (for
// example macOS /var), but never follows a symlink below that boundary.
// The final path itself may be a link: restoring replaces it without following.
func validateParents(root, destination string) error {
	rel, err := filepath.Rel(root, destination)
	if err != nil {
		return err
	}
	if !filepath.IsLocal(rel) {
		return fmt.Errorf("cache path %q escapes root %q", destination, root)
	}
	parent := filepath.Dir(rel)
	if parent == "." {
		return nil
	}
	current := root
	for _, part := range strings.Split(parent, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("cache path parent %q is not a directory or is a symlink", current)
		}
	}
	return nil
}

type outputArtifact struct {
	rel       string
	source    string
	directory bool
}

// Keep the original manifest indexes when normalizing older entries with
// redundant declarations. Directory snapshots already include their children.
func outputArtifacts(outputs Outputs, entryDir string, normalize bool) []outputArtifact {
	artifacts := make([]outputArtifact, 0, len(outputs.Files)+len(outputs.Dirs))
	seen := make(map[string]bool)
	appendOutput := func(rel, kind string, index int, directory bool) {
		rel = filepath.Clean(rel)
		if normalize {
			if seen[rel] {
				return
			}
			for _, dir := range outputs.Dirs {
				if strings.HasPrefix(rel, filepath.Clean(dir)+string(filepath.Separator)) {
					return
				}
			}
			seen[rel] = true
		}
		artifacts = append(artifacts, outputArtifact{rel: rel, source: filepath.Join(entryDir, kind, strconv.Itoa(index)), directory: directory})
	}
	for i, rel := range outputs.Files {
		appendOutput(rel, "files", i, false)
	}
	for i, rel := range outputs.Dirs {
		appendOutput(rel, "dirs", i, true)
	}
	return artifacts
}

func validateArtifact(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if directory && !info.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}
	if !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", path)
	}
	return nil
}

func restoreOutputs(ctx context.Context, worktree, entryDir string, outputs Outputs, onProgress func(fsutil.CopyProgress)) (bool, error) {
	artifacts := outputArtifacts(outputs, entryDir, true)
	for _, artifact := range artifacts {
		if err := validateParents(worktree, filepath.Join(worktree, artifact.rel)); err != nil {
			return false, err
		}
		if err := validateParents(entryDir, artifact.source); err != nil {
			_ = fsutil.RemoveAllWritable(entryDir)
			return false, nil
		}
		if err := validateArtifact(artifact.source, artifact.directory); err != nil {
			if os.IsPermission(err) {
				return false, err
			}
			_ = fsutil.RemoveAllWritable(entryDir)
			return false, nil
		}
	}
	// Stage on the destination filesystem and under ignored runtime metadata.
	// No declared output is removed until every cached artifact copies cleanly.
	stateDir := filepath.Join(worktree, ".devflow")
	if err := validateParents(worktree, filepath.Join(stateDir, "cache-restore")); err != nil {
		return false, err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return false, err
	}
	staging, err := os.MkdirTemp(stateDir, "cache-restore-")
	if err != nil {
		return false, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = fsutil.RemoveAllWritable(staging)
		}
	}()
	copier := fsutil.NewCopier(fsutil.CopyOptions{OnProgress: onProgress})
	for i, artifact := range artifacts {
		projection := filepath.Dir(artifact.source)
		if artifact.directory {
			projection = artifact.source
		}
		if err := copier.Copy(ctx, projection, artifact.source, filepath.Join(staging, "new", strconv.Itoa(i))); err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			var pathError *os.PathError
			if errors.As(err, &pathError) && !os.IsNotExist(err) {
				// Destination failures such as disk exhaustion do not make a
				// shared cache entry corrupt. Leave it available for retry.
				return false, err
			}
			_ = fsutil.RemoveAllWritable(entryDir)
			return false, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	backedUp := make([]bool, len(artifacts))
	installed := make([]bool, len(artifacts))
	rollback := func(cause error) (bool, error) {
		for i := len(artifacts) - 1; i >= 0; i-- {
			destination := filepath.Join(worktree, artifacts[i].rel)
			var err error
			if backedUp[i] {
				err = fsutil.MovePathWritable(filepath.Join(staging, "old", strconv.Itoa(i)), destination)
			} else if installed[i] {
				err = fsutil.RemoveAllWritable(destination)
			}
			if err != nil {
				cleanup = false
				cause = errors.Join(cause, fmt.Errorf("restore rollback failed; original outputs retained under %q: %w", staging, err))
			}
		}
		return false, cause
	}
	for i, artifact := range artifacts {
		if err := ctx.Err(); err != nil {
			return rollback(err)
		}
		destination := filepath.Join(worktree, artifact.rel)
		if err := validateParents(worktree, destination); err != nil {
			return rollback(err)
		}
		if _, err := os.Lstat(destination); err == nil {
			if err := fsutil.MovePathWritable(destination, filepath.Join(staging, "old", strconv.Itoa(i))); err != nil {
				return rollback(err)
			}
			backedUp[i] = true
		} else if !os.IsNotExist(err) {
			return rollback(err)
		}
		if err := fsutil.MovePathWritable(filepath.Join(staging, "new", strconv.Itoa(i)), destination); err != nil {
			return rollback(err)
		}
		installed[i] = true
	}
	return true, nil
}
