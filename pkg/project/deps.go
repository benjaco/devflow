package project

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"sort"

	"devflow/pkg/process"
)

type DependencyProvider interface {
	Dependencies() []Dependency
}

type Dependency struct {
	Name        string
	Command     string
	Description string
	Install     map[string]InstallScript
}

type InstallScript struct {
	Shell  string
	Script string
}

type DependencyStatus struct {
	Name        string `json:"name"`
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
	Installed   bool   `json:"installed"`
	Installable bool   `json:"installable"`
	Platform    string `json:"platform,omitempty"`
}

type DependencyInstallResult struct {
	Installed      []string `json:"installed"`
	AlreadyPresent []string `json:"alreadyPresent"`
	MissingInstall []string `json:"missingInstall"`
}

func DependenciesFor(p Project) []Dependency {
	provider, ok := p.(DependencyProvider)
	if !ok {
		return nil
	}
	deps := append([]Dependency(nil), provider.Dependencies()...)
	sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	return deps
}

func CheckDependencies(deps []Dependency) []DependencyStatus {
	statuses := make([]DependencyStatus, 0, len(deps))
	for _, dep := range deps {
		statuses = append(statuses, CheckDependency(dep))
	}
	return statuses
}

func CheckDependency(dep Dependency) DependencyStatus {
	_, err := exec.LookPath(dep.Command)
	script, ok := installScriptForPlatform(dep)
	return DependencyStatus{
		Name:        dep.Name,
		Command:     dep.Command,
		Description: dep.Description,
		Installed:   err == nil,
		Installable: ok && script.Script != "",
		Platform:    runtime.GOOS,
	}
}

func InstallMissingDependencies(ctx context.Context, workdir string, deps []Dependency, onLine func(string, string)) (DependencyInstallResult, error) {
	result := DependencyInstallResult{}
	for _, dep := range deps {
		if _, err := exec.LookPath(dep.Command); err == nil {
			result.AlreadyPresent = append(result.AlreadyPresent, dep.Name)
			continue
		}
		script, ok := installScriptForPlatform(dep)
		if !ok || script.Script == "" {
			result.MissingInstall = append(result.MissingInstall, dep.Name)
			continue
		}
		shell, args, err := shellForScript(script)
		if err != nil {
			return result, fmt.Errorf("dependency %q: %w", dep.Name, err)
		}
		args = append(args, script.Script)
		if _, err := process.Run(ctx, process.CommandSpec{
			Name:   shell,
			Args:   args,
			Dir:    workdir,
			OnLine: onLine,
		}); err != nil {
			return result, fmt.Errorf("install %q: %w", dep.Name, err)
		}
		if _, err := exec.LookPath(dep.Command); err != nil {
			return result, fmt.Errorf("install %q completed but command %q is still missing", dep.Name, dep.Command)
		}
		result.Installed = append(result.Installed, dep.Name)
	}
	if len(result.MissingInstall) > 0 {
		return result, fmt.Errorf("missing install scripts for: %s", joinStrings(result.MissingInstall, ", "))
	}
	return result, nil
}

func EnsureDependencyExists(dep Dependency) error {
	if _, err := exec.LookPath(dep.Command); err != nil {
		status := CheckDependency(dep)
		if status.Installable {
			return fmt.Errorf("required command %q not found; run `devflow deps install` for %s", dep.Command, dep.Name)
		}
		return fmt.Errorf("required command %q not found: %w", dep.Command, err)
	}
	return nil
}

func EnsureDependencies(deps []Dependency, names ...string) error {
	index := map[string]Dependency{}
	for _, dep := range deps {
		index[dep.Name] = dep
		index[dep.Command] = dep
	}
	for _, name := range names {
		dep, ok := index[name]
		if !ok {
			return fmt.Errorf("unknown dependency %q", name)
		}
		if err := EnsureDependencyExists(dep); err != nil {
			return err
		}
	}
	return nil
}

func installScriptForPlatform(dep Dependency) (InstallScript, bool) {
	if dep.Install == nil {
		return InstallScript{}, false
	}
	if script, ok := dep.Install[runtime.GOOS]; ok {
		return script, true
	}
	if runtime.GOOS != "windows" {
		if script, ok := dep.Install["unix"]; ok {
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
