package cli

import (
	"bufio"
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/benjaco/devflow/internal/clierror"
	"github.com/benjaco/devflow/internal/projectversion"
	"github.com/benjaco/devflow/internal/version"
	"golang.org/x/mod/module"
	"golang.org/x/term"
)

func confirmProjectUpgrade(ctx context.Context, input io.Reader, output io.Writer, worktree, previous, target string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := fmt.Fprintf(output, "Also update project %q from Devflow %s to %s? [y/N] ", worktree, previous, target); err != nil {
		return false, err
	}
	type answer struct {
		line string
		err  error
	}
	result := make(chan answer, 1)
	// A terminal read may outlive cancellation. Only one bounded reader is needed
	// for this command; the CLI can still exit promptly on Ctrl+C.
	go func() {
		line, err := bufio.NewReader(io.LimitReader(input, 257)).ReadString('\n')
		result <- answer{line, err}
	}()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case read := <-result:
		if len(read.line) > 256 {
			return false, fmt.Errorf("upgrade confirmation exceeds 256 bytes")
		}
		if errors.Is(read.err, io.EOF) {
			return false, nil
		}
		if read.err != nil {
			return false, read.err
		}
		value := strings.ToLower(strings.TrimSpace(read.line))
		return value == "y" || value == "yes", ctx.Err()
	}
}

func upgradeTerminal(input io.Reader, output io.Writer) bool {
	in, inOK := input.(*os.File)
	out, outOK := output.(*os.File)
	return inOK && outOK && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

func (a *App) selectUpgradeProject(worktree, target string, requested *bool, jsonOutput bool) (*projectversion.Selection, error) {
	// Headless upgrades never read stdin or infer permission to change a pin.
	if requested != nil && !*requested || requested == nil && (jsonOutput || !upgradeTerminal(a.Stdin, a.Stderr)) {
		return nil, nil
	}
	root, err := resolveWorktree(worktree)
	var selection *projectversion.Selection
	if err == nil {
		selection, err = projectversion.Resolve(root)
	}
	if err == nil && selection == nil && requested != nil {
		err = fmt.Errorf("project %q has no Devflow requirement in its root go.mod", root)
	}
	if err != nil {
		if requested != nil {
			return nil, clierror.Wrap(err, "invalid_project_version", "resolution")
		}
		_, _ = fmt.Fprintf(a.Stderr, "[devflow] project update unavailable: %v\n", err)
		return nil, nil
	}
	if selection == nil {
		return nil, nil
	}
	if selection.ReplacementPath != "" {
		err := fmt.Errorf("project has a Devflow replacement; update that replacement explicitly, or use --project=false to upgrade only the launcher")
		if requested != nil {
			return nil, clierror.Wrap(err, "project_update_failed", "resolution")
		}
		_, _ = fmt.Fprintf(a.Stderr, "[devflow] project update unavailable: %v\n", err)
		return nil, nil
	}
	if requested == nil {
		confirmed, err := confirmProjectUpgrade(a.context(), a.Stdin, a.Stderr, selection.Worktree, selection.Version, target)
		if err != nil {
			return nil, err
		}
		if !confirmed {
			_, _ = fmt.Fprintf(a.Stderr, "[devflow] project pin unchanged: %s\n", selection.Version)
			return nil, nil
		}
	}
	return selection, nil
}

func installedDevflowVersion(ctx context.Context) (string, error) {
	path, err := goInstalledDevflowPath(ctx, "go")
	if err != nil {
		return "", err
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read installed Devflow version at %s: %w", path, err)
	}
	// Resolve latest only once, through installation. PATH may still point at a
	// different launcher, so inspect the actual Go install destination directly.
	if info.Path != version.CommandPackage || info.Main.Path != version.ModulePath || info.Main.Replace != nil {
		return "", fmt.Errorf("installed binary at %s is not an official Devflow command", path)
	}
	if err := module.Check(info.Main.Path, info.Main.Version); err != nil {
		return "", fmt.Errorf("installed Devflow has no valid module version: %w", err)
	}
	return info.Main.Version, nil
}
