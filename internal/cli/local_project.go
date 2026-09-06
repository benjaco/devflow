package cli

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/benjaco/devflow/internal/clierror"
	"github.com/benjaco/devflow/internal/lock"
	"github.com/benjaco/devflow/internal/version"
	"github.com/benjaco/devflow/pkg/process"
)

const (
	envBootstrapEntry         = "DEVFLOW_BOOTSTRAP_ENTRY"
	envBootstrapRoot          = "DEVFLOW_BOOTSTRAP_ROOT"
	envBootstrapModuleVersion = "DEVFLOW_BOOTSTRAP_MODULE_VERSION"
	envLocalExec              = "DEVFLOW_LOCAL_EXEC"
	localProjectFile          = "devflow.project.go"
)

func shouldExecLocalProject(args []string, worktree string) bool {
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
	// Any filesystem object at the marker path enters bootstrap validation so a
	// directory, symlink, or special file produces the precise source error.
	_, err := os.Lstat(filepath.Join(worktree, localProjectFile))
	return err == nil
}

func (a *App) execLocalProject(args []string, worktree string) error {
	bootstrapRoot := bootstrapRoot()
	localBinary, err := ensureLocalProjectBinary(a.context(), bootstrapRoot, worktree)
	if err != nil {
		return err
	}
	env := withEnv(os.Environ(), envLocalExec, "1")
	env = withEnv(env, envBootstrapRoot, bootstrapRoot)
	return clierror.Wrap(execLocalBinary(a.context(), localBinary, append([]string{localBinary}, args...), env, a.Stdout, a.Stderr, a.localChildOwnsExecution), "bootstrap_failed", "bootstrap")
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

func ensureLocalProjectBinary(ctx context.Context, bootstrapRoot, worktree string) (binary string, err error) {
	defer func() { err = clierror.Wrap(err, "bootstrap_failed", "bootstrap") }()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	projectPath := filepath.Join(worktree, localProjectFile)
	target := filepath.Join(worktree, ".devflow", "bin", "devflow-local"+localProjectBinarySuffix())
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
	lockFile, err := lock.AcquireContext(ctx, localBuildLockPath(worktree))
	if err != nil {
		return "", err
	}
	defer lockFile.Release()
	buildKey, projectSources, err := localBuildKeyForBuild(bootstrapRoot, projectPath)
	if err != nil {
		return "", err
	}
	needsBuild, err = localBinaryNeedsBuild(target, buildKey)
	if err != nil {
		return "", err
	}
	if !needsBuild {
		return target, nil
	}
	if err := buildLocalProjectBinary(ctx, bootstrapRoot, worktree, projectSources, target, buildKey); err != nil {
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

func localProjectSourceFiles(projectPath string) (sources []string, err error) {
	defer func() { err = clierror.Wrap(err, "adapter_source_invalid", "bootstrap") }()
	// Keep project detection anchored to the explicit marker. This is intentionally
	// narrower than loading a Go package: unrelated root Go files and adapter tests
	// must not become part of the runtime CLI by accident.
	projectPath = filepath.Clean(projectPath)
	projectDir := filepath.Dir(projectPath)
	if err := validateLocalProjectSource(projectPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, clierror.Wrap(fmt.Errorf("%s not found in %s", localProjectFile, projectDir), "adapter_not_found", "bootstrap")
		}
		return nil, err
	}

	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return nil, fmt.Errorf("discover local project sources in %s: %w", projectDir, err)
	}
	companions := make([]string, 0)
	seen := map[string]struct{}{projectPath: {}}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "devflow_") || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(projectDir, name)
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		if err := validateLocalProjectSource(path); err != nil {
			return nil, err
		}
		seen[path] = struct{}{}
		companions = append(companions, path)
	}
	sort.Strings(companions)
	return append([]string{projectPath}, companions...), nil
}

func validateLocalProjectSource(path string) (err error) {
	defer func() { err = clierror.Wrap(err, "adapter_source_invalid", "bootstrap") }()
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("local project source %s is a symlink, expected a regular Go source file", path)
	case info.IsDir():
		return fmt.Errorf("local project source %s is a directory, expected a regular Go source file", path)
	case !info.Mode().IsRegular():
		return fmt.Errorf("local project source %s has mode %s, expected a regular Go source file", path, info.Mode())
	default:
		return nil
	}
}

func localBuildSources(bootstrapRoot, projectPath string) ([]string, error) {
	projectSources, err := localProjectSourceFiles(projectPath)
	if err != nil {
		return nil, err
	}
	return localBuildSourcesForProject(bootstrapRoot, projectSources)
}

func localBuildSourcesForProject(bootstrapRoot string, projectSources []string) ([]string, error) {
	sources := append([]string(nil), projectSources...)
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
	return localBuildKeyFromSources(bootstrapRoot, sources)
}

func localBuildKeyForBuild(bootstrapRoot, projectPath string) (string, []string, error) {
	// Rediscover after taking localbuild.lock, then use this exact ordered source
	// set for both the content key and generated module.
	projectSources, err := localProjectSourceFiles(projectPath)
	if err != nil {
		return "", nil, err
	}
	sources, err := localBuildSourcesForProject(bootstrapRoot, projectSources)
	if err != nil {
		return "", nil, err
	}
	key, err := localBuildKeyFromSources(bootstrapRoot, sources)
	if err != nil {
		return "", nil, err
	}
	return key, projectSources, nil
}

func localBuildKeyFromSources(bootstrapRoot string, sources []string) (string, error) {
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
		if _, err := hash.Write([]byte(localBuildSourceLabel(bootstrapRoot, path))); err != nil {
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

func localBuildSourceLabel(bootstrapRoot, path string) string {
	if bootstrapRoot == "" {
		return filepath.ToSlash(path)
	}
	rel, err := filepath.Rel(bootstrapRoot, path)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return filepath.ToSlash(rel)
	}
	return "external/" + filepath.ToSlash(path)
}

func buildLocalProjectBinary(ctx context.Context, bootstrapRoot, worktree string, projectSources []string, target, buildKey string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(projectSources) == 0 {
		return fmt.Errorf("cannot build local devflow binary without %s", localProjectFile)
	}
	projectPath := projectSources[0]
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
	for _, sourcePath := range projectSources {
		if err := validateLocalProjectSource(sourcePath); err != nil {
			return err
		}
		name := filepath.Base(sourcePath)
		if name == "main.go" || name == "go.mod" || name == "go.sum" {
			return fmt.Errorf("local project source %s conflicts with generated local-build file %s", sourcePath, name)
		}
		data, err := os.ReadFile(sourcePath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(buildDir, name), data, 0o644); err != nil {
			return err
		}
	}
	cmd := process.CommandContext(ctx, "go", "build", "-mod=mod", "-o", tmpTarget, ".")
	cmd.Dir = buildDir
	output, err := cmd.CombinedOutput()
	// A canceled build must never publish an artifact, even if the child exits zero.
	if ctx.Err() != nil {
		_ = os.Remove(tmpTarget)
		return ctx.Err()
	}
	if err != nil {
		_ = os.Remove(tmpTarget)
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return clierror.Wrap(fmt.Errorf("failed to build local devflow binary from %s: %w", projectPath, err), "adapter_compile_failed", "bootstrap")
		}
		return clierror.Wrap(fmt.Errorf("failed to build local devflow binary from %s: %w\n%s", projectPath, err, trimmed), "adapter_compile_failed", "bootstrap")
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

func localBuildLockPath(worktree string) string {
	return filepath.Join(worktree, ".devflow", "localbuild.lock")
}

func localBuildMainSource() string {
	return `package main

import (
	"os"

	"github.com/benjaco/devflow/internal/cli"
)

func main() {
	app := cli.New()
	if err := app.Run(os.Args[1:]); err != nil {
		cli.ReportError(os.Stderr, err)
		os.Exit(cli.ExitCode(err))
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
	_, _ = fmt.Fprintln(&b, "go 1.27.1")
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

func localProjectBinarySuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func isProjectlessCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "logs", "runs", "prompts", "version", "upgrade", "docs", "instances":
		return true
	default:
		return false
	}
}
