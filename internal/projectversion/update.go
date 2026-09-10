package projectversion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/internal/lock"
	"github.com/benjaco/devflow/pkg/process"
	"golang.org/x/mod/modfile"
)

// Update changes an explicitly approved project pin through Go's module solver.
// Go works on staged files; failed downloads, cancellation and concurrent edits
// leave the application's module files intact.
func Update(ctx context.Context, selection *Selection, targetVersion string, env []string, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if selection == nil {
		return errors.New("no project Devflow version selected")
	}
	if err := exactVersion(ModulePath, targetVersion); err != nil {
		return err
	}
	if selection.ReplacementPath != "" {
		return errors.New("project Devflow has a replacement; update that source policy explicitly")
	}
	module, err := modfile.Parse("go.mod", selection.modBytes, nil)
	if err != nil {
		return err
	}
	if err := rejectUpdateReplacement(module, targetVersion); err != nil {
		return err
	}
	versions := filepath.Join(selection.Worktree, ".devflow", "versions")
	lease, err := lock.AcquireContext(ctx, filepath.Join(versions, "prepare.lock"))
	if err != nil {
		return err
	}
	defer lease.Release()
	if err := checkSelection(selection); err != nil {
		return err
	}
	modulePath, sumPath := filepath.Join(selection.Worktree, "go.mod"), filepath.Join(selection.Worktree, "go.sum")
	moduleInfo, err := os.Lstat(modulePath)
	if err != nil {
		return err
	}
	if !moduleInfo.Mode().IsRegular() {
		return fmt.Errorf("project module file %s is not a regular file", modulePath)
	}
	sumMode := os.FileMode(0o644)
	sumExists := false
	if info, err := os.Lstat(sumPath); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("project checksum file %s is not a regular file", sumPath)
		}
		sumMode, sumExists = info.Mode().Perm(), true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	staging, err := os.MkdirTemp(versions, "update-")
	if err != nil {
		return err
	}
	keepStaging := false
	defer func() {
		if !keepStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	stagedModule, stagedSum := filepath.Join(staging, "update.mod"), filepath.Join(staging, "update.sum")
	if err := os.WriteFile(stagedModule, selection.modBytes, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(stagedSum, selection.sumBytes, 0o600); err != nil {
		return err
	}
	backupSum := filepath.Join(staging, "original.sum")
	if sumExists {
		if err := os.WriteFile(backupSum, selection.sumBytes, sumMode); err != nil {
			return err
		}
	}
	if env == nil {
		env = os.Environ()
	}
	if stderr == nil {
		stderr = io.Discard
	}
	// -modfile changes the files Go may write, while cwd keeps application-relative
	// replacements rooted at the real project instead of the staging directory.
	command := process.CommandContext(ctx, "go", "get", "-modfile="+stagedModule, ModulePath+"@"+targetVersion)
	command.Dir = selection.Worktree
	command.Env = BuildEnv(env)
	var diagnostic tailBuffer
	output := io.MultiWriter(stderr, &diagnostic)
	command.Stdout, command.Stderr = output, output
	err = command.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("update project Devflow to %s: %w%s", targetVersion, err, diagnostic.suffix())
	}
	data, err := os.ReadFile(stagedModule)
	if err != nil {
		return err
	}
	updated, err := modfile.Parse(stagedModule, data, nil)
	if err != nil {
		return err
	}
	if err := rejectUpdateReplacement(updated, targetVersion); err != nil {
		return err
	}
	var selectedVersion string
	for _, require := range updated.Require {
		if require.Mod.Path == ModulePath {
			selectedVersion = require.Mod.Version
		}
	}
	if selectedVersion != targetVersion {
		return fmt.Errorf("module solver selected project Devflow %q; expected exact version %s", selectedVersion, targetVersion)
	}
	if err := checkSelection(selection); err != nil {
		return err
	}
	if err := os.Chmod(stagedModule, moduleInfo.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chmod(stagedSum, sumMode); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A pair of files has no atomic rename. Publish checksums before the pin, then
	// finish or roll back this short commit even if cancellation arrives meanwhile.
	if err := fsutil.ReplaceFile(stagedSum, sumPath); err != nil {
		return err
	}
	if err := fsutil.ReplaceFile(stagedModule, modulePath); err != nil {
		var rollbackErr error
		if sumExists {
			rollbackErr = fsutil.ReplaceFile(backupSum, sumPath)
		} else {
			rollbackErr = os.Remove(sumPath)
		}
		if rollbackErr != nil {
			keepStaging = true
			return errors.Join(err, fmt.Errorf("restore project checksums failed; originals retained at %s: %w", staging, rollbackErr))
		}
		return err
	}
	return nil
}

func rejectUpdateReplacement(module *modfile.File, target string) error {
	for _, replacement := range module.Replace {
		if replacement.Old.Path == ModulePath && (replacement.Old.Version == "" || replacement.Old.Version == target) {
			return fmt.Errorf("project Devflow %s has a replacement; update that source policy explicitly", target)
		}
	}
	return nil
}
