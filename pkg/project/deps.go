package project

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"sort"

	"github.com/benjaco/devflow/pkg/process"
)

type RequiredCLIProvider interface {
	RequiredCLIs() []RequiredCLI
}

type RequiredCLI struct {
	Name        string
	Command     string
	Description string
	Install     map[string]InstallScript
}

// DependencyProvider is kept for compatibility with early adapters.
// New adapters should implement RequiredCLIProvider instead.
type DependencyProvider interface {
	Dependencies() []Dependency
}

// Dependency is a compatibility alias for RequiredCLI.
type Dependency = RequiredCLI

type InstallScript struct {
	Shell  string
	Script string
}

type RequiredCLIStatus struct {
	Name        string `json:"name"`
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
	Installed   bool   `json:"installed"`
	Installable bool   `json:"installable"`
	Platform    string `json:"platform,omitempty"`
}

type DependencyStatus = RequiredCLIStatus

type RequiredCLIInstallResult struct {
	Installed      []string `json:"installed"`
	AlreadyPresent []string `json:"alreadyPresent"`
	MissingInstall []string `json:"missingInstall"`
}

type DependencyInstallResult = RequiredCLIInstallResult

func RequiredCLIsFor(p Project) []RequiredCLI {
	if provider, ok := p.(RequiredCLIProvider); ok {
		clis := append([]RequiredCLI(nil), provider.RequiredCLIs()...)
		sort.Slice(clis, func(i, j int) bool { return clis[i].Name < clis[j].Name })
		return clis
	}
	if provider, ok := p.(DependencyProvider); ok {
		clis := append([]RequiredCLI(nil), provider.Dependencies()...)
		sort.Slice(clis, func(i, j int) bool { return clis[i].Name < clis[j].Name })
		return clis
	}
	return nil
}

func DependenciesFor(p Project) []Dependency {
	return RequiredCLIsFor(p)
}

func RequiredCLIsForTarget(p Project, target string) ([]RequiredCLI, error) {
	targetDef, ok := targetByName(p.Targets(), target)
	if !ok {
		return nil, fmt.Errorf("unknown target %q", target)
	}
	catalog := RequiredCLIsFor(p)
	index := requiredCLIIndex(catalog)
	selected := map[string]RequiredCLI{}
	add := func(name string) error {
		cli, ok := index[name]
		if !ok {
			return fmt.Errorf("unknown required CLI %q", name)
		}
		selected[cli.Name] = cli
		return nil
	}
	for _, name := range targetDef.RequiredCLIs {
		if err := add(name); err != nil {
			return nil, fmt.Errorf("target %q: %w", targetDef.Name, err)
		}
	}

	tasks := map[string]Task{}
	for _, task := range p.Tasks() {
		tasks[task.Name] = task
	}
	state := map[string]int{}
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("cycle detected at task %q", name)
		case 2:
			return nil
		}
		task, ok := tasks[name]
		if !ok {
			return fmt.Errorf("target %q references missing task %q", targetDef.Name, name)
		}
		state[name] = 1
		for _, cliName := range task.RequiredCLIs {
			if err := add(cliName); err != nil {
				return fmt.Errorf("task %q: %w", task.Name, err)
			}
		}
		for _, dep := range task.Deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[name] = 2
		return nil
	}
	for _, root := range targetDef.RootTasks {
		if err := visit(root); err != nil {
			return nil, err
		}
	}
	return sortedRequiredCLIs(selected), nil
}

func DependenciesForTarget(p Project, target string) ([]Dependency, error) {
	return RequiredCLIsForTarget(p, target)
}

func CheckRequiredCLIs(clis []RequiredCLI) []RequiredCLIStatus {
	statuses := make([]RequiredCLIStatus, 0, len(clis))
	for _, cli := range clis {
		statuses = append(statuses, CheckRequiredCLI(cli))
	}
	return statuses
}

func CheckDependencies(deps []Dependency) []DependencyStatus {
	return CheckRequiredCLIs(deps)
}

func CheckRequiredCLI(cli RequiredCLI) RequiredCLIStatus {
	_, err := exec.LookPath(cli.Command)
	script, ok := installScriptForPlatform(cli)
	return RequiredCLIStatus{
		Name:        cli.Name,
		Command:     cli.Command,
		Description: cli.Description,
		Installed:   err == nil,
		Installable: ok && script.Script != "",
		Platform:    runtime.GOOS,
	}
}

func CheckDependency(dep Dependency) DependencyStatus {
	return CheckRequiredCLI(dep)
}

func InstallMissingRequiredCLIs(ctx context.Context, workdir string, clis []RequiredCLI, onLine func(string, string)) (RequiredCLIInstallResult, error) {
	result := RequiredCLIInstallResult{}
	for _, cli := range clis {
		if _, err := exec.LookPath(cli.Command); err == nil {
			result.AlreadyPresent = append(result.AlreadyPresent, cli.Name)
			continue
		}
		script, ok := installScriptForPlatform(cli)
		if !ok || script.Script == "" {
			result.MissingInstall = append(result.MissingInstall, cli.Name)
			continue
		}
		shell, args, err := shellForScript(script)
		if err != nil {
			return result, fmt.Errorf("required CLI %q: %w", cli.Name, err)
		}
		args = append(args, script.Script)
		if _, err := process.Run(ctx, process.CommandSpec{
			Name:   shell,
			Args:   args,
			Dir:    workdir,
			OnLine: onLine,
		}); err != nil {
			return result, fmt.Errorf("install %q: %w", cli.Name, err)
		}
		if _, err := exec.LookPath(cli.Command); err != nil {
			return result, fmt.Errorf("install %q completed but command %q is still missing", cli.Name, cli.Command)
		}
		result.Installed = append(result.Installed, cli.Name)
	}
	if len(result.MissingInstall) > 0 {
		return result, fmt.Errorf("missing install scripts for: %s", joinStrings(result.MissingInstall, ", "))
	}
	return result, nil
}

func InstallMissingDependencies(ctx context.Context, workdir string, deps []Dependency, onLine func(string, string)) (DependencyInstallResult, error) {
	return InstallMissingRequiredCLIs(ctx, workdir, deps, onLine)
}

func EnsureRequiredCLIExists(cli RequiredCLI) error {
	if _, err := exec.LookPath(cli.Command); err != nil {
		status := CheckRequiredCLI(cli)
		if status.Installable {
			return fmt.Errorf("required CLI %q not found; run `devflow clis install` for %s", cli.Command, cli.Name)
		}
		return fmt.Errorf("required CLI %q not found: %w", cli.Command, err)
	}
	return nil
}

func EnsureDependencyExists(dep Dependency) error {
	return EnsureRequiredCLIExists(dep)
}

func EnsureRequiredCLIs(clis []RequiredCLI, names ...string) error {
	index := requiredCLIIndex(clis)
	for _, name := range names {
		cli, ok := index[name]
		if !ok {
			return fmt.Errorf("unknown required CLI %q", name)
		}
		if err := EnsureRequiredCLIExists(cli); err != nil {
			return err
		}
	}
	return nil
}

func EnsureDependencies(deps []Dependency, names ...string) error {
	return EnsureRequiredCLIs(deps, names...)
}

func targetByName(targets []Target, name string) (Target, bool) {
	for _, target := range targets {
		if target.Name == name {
			return target, true
		}
	}
	return Target{}, false
}

func requiredCLIIndex(clis []RequiredCLI) map[string]RequiredCLI {
	index := map[string]RequiredCLI{}
	for _, cli := range clis {
		index[cli.Name] = cli
		index[cli.Command] = cli
	}
	return index
}

func sortedRequiredCLIs(items map[string]RequiredCLI) []RequiredCLI {
	clis := make([]RequiredCLI, 0, len(items))
	for _, cli := range items {
		clis = append(clis, cli)
	}
	sort.Slice(clis, func(i, j int) bool { return clis[i].Name < clis[j].Name })
	return clis
}

func installScriptForPlatform(cli RequiredCLI) (InstallScript, bool) {
	if cli.Install == nil {
		return InstallScript{}, false
	}
	if script, ok := cli.Install[runtime.GOOS]; ok {
		return script, true
	}
	if runtime.GOOS != "windows" {
		if script, ok := cli.Install["unix"]; ok {
			return script, true
		}
	}
	return InstallScript{}, false
}

func shellForScript(script InstallScript) (string, []string, error) {
	if script.Shell != "" {
		switch script.Shell {
		case "sh":
			return "sh", []string{"-c"}, nil
		case "bash":
			return "bash", []string{"-c"}, nil
		case "powershell":
			return "powershell", []string{"-Command"}, nil
		case "pwsh":
			return "pwsh", []string{"-Command"}, nil
		default:
			return "", nil, fmt.Errorf("unsupported installer shell %q", script.Shell)
		}
	}
	if runtime.GOOS == "windows" {
		return "powershell", []string{"-Command"}, nil
	}
	return "sh", []string{"-c"}, nil
}

func joinStrings(items []string, sep string) string {
	if len(items) == 0 {
		return ""
	}
	out := items[0]
	for _, item := range items[1:] {
		out += sep + item
	}
	return out
}
