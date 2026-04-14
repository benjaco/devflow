package cli

import (
	"crypto/sha1"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	envBootstrapEntry = "DEVFLOW_BOOTSTRAP_ENTRY"
	envBootstrapRoot  = "DEVFLOW_BOOTSTRAP_ROOT"
	envLocalExec      = "DEVFLOW_LOCAL_EXEC"
	localProjectFile  = "devflow.project.go"
)

func shouldExecLocalProject(args []string) bool {
	if os.Getenv(envBootstrapEntry) != "1" {
		return false
	}
	if os.Getenv(envLocalExec) == "1" {
		return false
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "__internal_") {
		return false
	}
	return true
}

func (a *App) execLocalProject(args []string) error {
	bootstrapRoot, err := bootstrapRoot()
	if err != nil {
		return err
	}
	worktree, err := worktreeFromArgs(args)
	if err != nil {
		return err
	}
	localBinary, err := ensureLocalProjectBinary(bootstrapRoot, worktree)
	if err != nil {
		return err
	}
	env := withEnv(os.Environ(), envLocalExec, "1")
	env = withEnv(env, envBootstrapRoot, bootstrapRoot)
	return syscall.Exec(localBinary, append([]string{localBinary}, args...), env)
}

func bootstrapRoot() (string, error) {
	root := strings.TrimSpace(os.Getenv(envBootstrapRoot))
	if root == "" {
		return "", fmt.Errorf("%s is not set; launch devflow through the repo launcher", envBootstrapRoot)
	}
	return filepath.Abs(root)
}

func worktreeFromArgs(args []string) (string, error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--worktree":
			if i+1 >= len(args) {
				return "", fmt.Errorf("missing value for --worktree")
			}
			return resolveWorktree(args[i+1])
		case strings.HasPrefix(arg, "--worktree="):
			return resolveWorktree(strings.TrimPrefix(arg, "--worktree="))
		}
	}
	return resolveWorktree("")
}

func ensureLocalProjectBinary(bootstrapRoot, worktree string) (string, error) {
	projectPath := filepath.Join(worktree, localProjectFile)
	info, err := os.Stat(projectPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%s not found in %s", localProjectFile, worktree)
		}
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s in %s is a directory, expected a Go source file", localProjectFile, worktree)
	}

	target := filepath.Join(worktree, ".devflow", "bin", "devflow-local")
	needsBuild, err := localBinaryNeedsBuild(bootstrapRoot, projectPath, target)
	if err != nil {
		return "", err
	}
	if !needsBuild {
		return target, nil
	}
	if err := buildLocalProjectBinary(bootstrapRoot, worktree, projectPath, target); err != nil {
		return "", err
	}
	return target, nil
}

func localBinaryNeedsBuild(bootstrapRoot, projectPath, target string) (bool, error) {
	targetInfo, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	sources, err := localBuildSources(bootstrapRoot, projectPath)
	if err != nil {
		return false, err
	}
	for _, path := range sources {
		info, err := os.Stat(path)
		if err != nil {
			return false, err
		}
		if info.ModTime().After(targetInfo.ModTime()) {
			return true, nil
		}
	}
	return false, nil
}

func localBuildSources(bootstrapRoot, projectPath string) ([]string, error) {
	sources := []string{
		projectPath,
		filepath.Join(bootstrapRoot, "go.mod"),
		filepath.Join(bootstrapRoot, "go.sum"),
	}
	for _, dir := range []string{"cmd", "internal", "pkg"} {
		root := filepath.Join(bootstrapRoot, dir)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if filepath.Ext(path) == ".go" {
				sources = append(sources, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return sources, nil
}

func buildLocalProjectBinary(bootstrapRoot, worktree, projectPath, target string) error {
	buildDir := localBuildDir(bootstrapRoot, worktree)
	if err := os.RemoveAll(buildDir); err != nil {
		return err
	}
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(buildDir, "main.go"), []byte(localBuildMainSource()), 0o644); err != nil {
		return err
	}
	data, err := os.ReadFile(projectPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(buildDir, localProjectFile), data, 0o644); err != nil {
		return err
	}
	rel, err := filepath.Rel(bootstrapRoot, buildDir)
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-o", target, "./"+filepath.ToSlash(rel))
	cmd.Dir = bootstrapRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return fmt.Errorf("failed to build local devflow binary from %s: %w", projectPath, err)
		}
		return fmt.Errorf("failed to build local devflow binary from %s: %w\n%s", projectPath, err, trimmed)
	}
	return nil
}

func localBuildDir(bootstrapRoot, worktree string) string {
	sum := sha1.Sum([]byte(worktree))
	return filepath.Join(bootstrapRoot, ".devflow", "localbuild", fmt.Sprintf("%x", sum[:6]))
}

func localBuildMainSource() string {
	return `package main

import (
	"fmt"
	"os"

	"devflow/internal/cli"
)

func main() {
	app := cli.New()
	if err := app.Run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`
}

func withEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			out = append(out, prefix+value)
			replaced = true
			continue
		}
		out = append(out, item)
	}
	if !replaced {
		out = append(out, prefix+value)
	}
	return out
}
