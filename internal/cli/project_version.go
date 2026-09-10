package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/benjaco/devflow/internal/clierror"
	"github.com/benjaco/devflow/internal/projectversion"
	"github.com/benjaco/devflow/internal/version"
)

const (
	envProjectRuntime  = "DEVFLOW_PROJECT_RUNTIME"
	envLauncherVersion = "DEVFLOW_LAUNCHER_VERSION"
)

// A launcher can know flag arities from its own release, but cannot validate a
// newer command. Routing flags remain available even after an unknown option.
func invocationFlag(args []string, known *flag.FlagSet, name string, takesValue bool) (string, bool) {
	value, found := "", false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		key, v, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if key == name {
			if !hasValue {
				v = "true"
				if takesValue {
					if i+1 == len(args) {
						continue
					}
					i++
					v = args[i]
				}
			}
			value, found = v, true
		} else if !hasValue && valueFlag(known, key) {
			i++
		}
	}
	return value, found
}

func runningExecutable(path string) bool {
	if path == "" {
		return false
	}
	current, err := os.Executable()
	if err != nil {
		return false
	}
	left, err := os.Stat(current)
	if err != nil {
		return false
	}
	right, err := os.Stat(path)
	return err == nil && os.SameFile(left, right)
}

func invocationWorktree(args []string, known *flag.FlagSet) (string, error) {
	worktreeFlag, _ := invocationFlag(args, known, "worktree", true)
	if instanceID, _ := invocationFlag(args, known, "instance", true); instanceID != "" {
		root, _, err := resolveInstance(worktreeFlag, instanceID)
		return root, err
	}
	return resolveWorktree(worktreeFlag)
}

// Handoff selects the complete CLI, including its parser and adapter generator.
// Merely changing the generated module's requirement would still run old code
// before bootstrap and reject commands introduced by the selected release.
func (a *App) execProjectVersion(args []string, known *flag.FlagSet) (bool, error) {
	if runningExecutable(os.Getenv(envLocalExec)) || (len(args) > 0 && (args[0] == "upgrade" || strings.HasPrefix(args[0], "__internal_"))) {
		return false, nil
	}
	selectedEntry := runningExecutable(os.Getenv(envProjectRuntime))
	if bootstrapRoot() != "" && !selectedEntry {
		return false, nil
	}
	root, err := invocationWorktree(args, known)
	if err != nil {
		return true, clierror.Wrap(err, "invalid_worktree", "resolution")
	}
	selection, err := projectversion.Resolve(root)
	if err != nil {
		return true, clierror.Wrap(err, "invalid_project_version", "bootstrap")
	}
	if selection == nil {
		if selectedEntry {
			return true, clierror.Wrap(fmt.Errorf("project Devflow pin disappeared during handoff; run devflow again"), "project_version_changed", "bootstrap")
		}
		return false, nil
	}
	if runningExecutable(projectversion.BinaryPath(selection)) {
		a.projectVersion = selection.Version
		a.launcherVersion = os.Getenv(envLauncherVersion)
		return false, nil
	}
	progress := a.Stderr
	if level, _ := invocationFlag(args, known, "progress", true); level == "quiet" {
		progress = io.Discard
	} else if a.githubActions {
		// Compiler output is progress, not permission to issue workflow commands.
		progress = projectVersionProgress{out: progress}
	}
	binary, err := projectversion.Prepare(a.context(), selection, progress)
	if err != nil {
		return true, clierror.Wrap(err, "project_version_prepare_failed", "bootstrap")
	}
	launcher := version.Current().Version
	if selectedEntry {
		launcher = os.Getenv(envLauncherVersion)
	}
	env := withEnv(os.Environ(), envProjectRuntime, binary)
	env = withEnv(env, envLauncherVersion, launcher)
	// Source replacements belong to the pin's module, not the caller's explicit
	// development override. Inheriting a managed override would bypass later pins.
	env = withEnv(env, envBootstrapRoot, "")
	return true, clierror.Wrap(execLocalBinary(a.context(), binary, append([]string{binary}, args...), env, a.Stdout, a.Stderr, a.localChildOwnsExecution), "bootstrap_failed", "bootstrap")
}

type projectVersionProgress struct{ out io.Writer }

func (w projectVersionProgress) Write(data []byte) (int, error) {
	// Separate every delimiter byte so commands cannot form across writes either.
	text := strings.NewReplacer(":", ": ", "#", "# ").Replace(string(data))
	if _, err := io.WriteString(w.out, text); err != nil {
		return 0, err
	}
	return len(data), nil
}
