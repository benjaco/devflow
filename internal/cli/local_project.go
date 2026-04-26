package cli

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/benjaco/devflow/internal/version"
)

const (
	envBootstrapEntry         = "DEVFLOW_BOOTSTRAP_ENTRY"
	envBootstrapRoot          = "DEVFLOW_BOOTSTRAP_ROOT"
	envBootstrapModuleVersion = "DEVFLOW_BOOTSTRAP_MODULE_VERSION"
	envLocalExec              = "DEVFLOW_LOCAL_EXEC"
	localProjectFile          = "devflow.project.go"
)

func shouldExecLocalProject(args []string) bool {
	if os.Getenv(envLocalExec) == "1" {
		return false
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "__internal_") {
		return false
	}
	if isProjectlessCommand(args) {
		return false
	}
	if os.Getenv(envBootstrapEntry) == "1" {
		return true
	}
	worktree, err := worktreeFromArgs(args)
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(worktree, localProjectFile))
	return err == nil && !info.IsDir()
}

func (a *App) execLocalProject(args []string) error {
	bootstrapRoot := bootstrapRoot()
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

func bootstrapRoot() string {
	root := strings.TrimSpace(os.Getenv(envBootstrapRoot))
	if root == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return root
	}
	return abs
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
	buildKey, err := localBuildKey(bootstrapRoot, projectPath)
	if err != nil {
		return "", err
	}
	needsBuild, err := localBinaryNeedsBuild(target, buildKey)
	if err != nil {
		return "", err
	}
	if !needsBuild {
		return target, nil
	}
	if err := buildLocalProjectBinary(bootstrapRoot, worktree, projectPath, target, buildKey); err != nil {
		return "", err
	}
	return target, nil
}

func localBinaryNeedsBuild(target, buildKey string) (bool, error) {
	targetInfo, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	keyPath := localBuildKeyPath(target)
	data, err := os.ReadFile(keyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	if strings.TrimSpace(string(data)) != buildKey {
		return true, nil
	}
	if targetInfo.Size() == 0 {
		return true, nil
	}
	return false, nil
}

func localBuildSources(bootstrapRoot, projectPath string) ([]string, error) {
	sources := []string{
		projectPath,
	}
	if bootstrapRoot == "" {
		return sources, nil
	}
	sources = append(sources,
		filepath.Join(bootstrapRoot, "go.mod"),
		filepath.Join(bootstrapRoot, "go.sum"),
	)
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

func localBuildKey(bootstrapRoot, projectPath string) (string, error) {
	sources, err := localBuildSources(bootstrapRoot, projectPath)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := hash.Write([]byte(version.Current().Version)); err != nil {
		return "", err
	}
	if _, err := hash.Write([]byte{0}); err != nil {
		return "", err
	}
	if _, err := hash.Write([]byte(bootstrapRoot)); err != nil {
		return "", err
	}
	if _, err := hash.Write([]byte{0}); err != nil {
		return "", err
	}
	for _, path := range sources {
		rel := path
		if bootstrapRoot != "" {
			var err error
			rel, err = filepath.Rel(bootstrapRoot, path)
			if err != nil {
				return "", err
			}
		}
		if _, err := hash.Write([]byte(filepath.ToSlash(rel))); err != nil {
			return "", err
		}
		if _, err := hash.Write([]byte{0}); err != nil {
			return "", err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if _, err := hash.Write(data); err != nil {
			return "", err
		}
		if _, err := hash.Write([]byte{0}); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func buildLocalProjectBinary(bootstrapRoot, worktree, projectPath, target, buildKey string) error {
	buildDir := localBuildDir(worktree)
	if err := os.RemoveAll(buildDir); err != nil {
		return err
	}
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmpTarget := target + ".tmp"
	_ = os.Remove(tmpTarget)
	if err := os.WriteFile(filepath.Join(buildDir, "main.go"), []byte(localBuildMainSource()), 0o644); err != nil {
		return err
	}
	moduleSource, err := localBuildModuleSource(buildDir, bootstrapRoot)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(buildDir, "go.mod"), []byte(moduleSource), 0o644); err != nil {
		return err
	}
	if bootstrapRoot != "" {
		if data, err := os.ReadFile(filepath.Join(bootstrapRoot, "go.sum")); err == nil {
			if err := os.WriteFile(filepath.Join(buildDir, "go.sum"), data, 0o644); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	data, err := os.ReadFile(projectPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(buildDir, localProjectFile), data, 0o644); err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-mod=mod", "-o", tmpTarget, ".")
	cmd.Dir = buildDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(tmpTarget)
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return fmt.Errorf("failed to build local devflow binary from %s: %w", projectPath, err)
		}
		return fmt.Errorf("failed to build local devflow binary from %s: %w\n%s", projectPath, err, trimmed)
	}
	if err := os.Rename(tmpTarget, target); err != nil {
		_ = os.Remove(tmpTarget)
		return err
	}
	return writeBuildKey(target, buildKey)
}

func localBuildDir(worktree string) string {
	sum := sha1.Sum([]byte(worktree))
	return filepath.Join(worktree, ".devflow", "localbuild", fmt.Sprintf("%x", sum[:6]))
}

func localBuildMainSource() string {
	return `package main

import (
	"fmt"
	"os"

	"github.com/benjaco/devflow/internal/cli"
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

func localBuildModuleSource(buildDir, bootstrapRoot string) (string, error) {
	modulePath := version.ModulePath + "/localbuild/" + filepath.Base(buildDir)
	requireVersion := localBuildRequireVersion(bootstrapRoot)
	if requireVersion == "" || requireVersion == "devel" {
		return "", fmt.Errorf("cannot build local devflow project from a development binary without %s; use the repo launcher or install with go install %s@latest", envBootstrapRoot, version.CommandPackage)
	}
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "module %s\n\n", modulePath)
	_, _ = fmt.Fprintln(&b, "go 1.23")
	_, _ = fmt.Fprintf(&b, "\nrequire %s %s\n", version.ModulePath, requireVersion)
	if bootstrapRoot != "" {
		_, _ = fmt.Fprintf(&b, "\nreplace %s => %s\n", version.ModulePath, filepath.ToSlash(bootstrapRoot))
	}
	return b.String(), nil
}

func localBuildRequireVersion(bootstrapRoot string) string {
	if bootstrapRoot != "" {
		return "v0.0.0"
	}
	if override := strings.TrimSpace(os.Getenv(envBootstrapModuleVersion)); override != "" {
		return override
	}
	return version.Current().Version
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

func localBuildKeyPath(target string) string {
	return target + ".buildkey"
}

func writeBuildKey(target, buildKey string) error {
	path := localBuildKeyPath(target)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(buildKey+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func isProjectlessCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "version", "upgrade", "instances":
		return true
	default:
		return false
	}
}
