package projectversion

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/benjaco/devflow/internal/lock"
	"github.com/benjaco/devflow/pkg/process"
)

// Prepare builds the selected module's own entrypoint, so its flag parser and
// adapter bootstrap belong to the project version before either receives argv.
func Prepare(ctx context.Context, selection *Selection, stderr io.Writer) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if selection == nil {
		return "", errors.New("no project Devflow version selected")
	}
	if stderr == nil {
		stderr = io.Discard
	}
	versions := filepath.Join(selection.Worktree, ".devflow", "versions")
	lease, err := lock.AcquireContext(ctx, filepath.Join(versions, "prepare.lock"))
	if err != nil {
		return "", err
	}
	defer lease.Release()
	if err := checkSelection(selection); err != nil {
		return "", err
	}
	binary := BinaryPath(selection)
	if info, err := os.Stat(binary); err == nil {
		if !info.Mode().IsRegular() || info.Size() == 0 || (runtime.GOOS != "windows" && info.Mode()&0o111 == 0) {
			return "", fmt.Errorf("project runtime %s is not a nonempty executable file", binary)
		}
		if err := verifyBinary(binary, selection); err != nil {
			return "", err
		}
		return binary, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	staging, err := os.MkdirTemp(versions, "build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	module, err := selection.ModuleSource("devflow.local/runtime")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(staging, "go.mod"), module, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(staging, "go.sum"), selection.sumBytes, 0o600); err != nil {
		return "", err
	}
	temporary := filepath.Join(staging, filepath.Base(binary))
	fmt.Fprintf(stderr, "[devflow] preparing project version %s\n", selection.Version)
	command := process.CommandContext(ctx, "go", "build", "-mod=mod", "-o", temporary, ModulePath+"/cmd/devflow")
	command.Dir = staging
	command.Env = BuildEnv(os.Environ())
	var diagnostic tailBuffer
	output := io.MultiWriter(stderr, &diagnostic)
	command.Stdout, command.Stderr = output, output
	err = command.Run()
	// Cancellation and source edits must never publish a binary under an identity
	// that no longer describes the invocation's selection.
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("build project Devflow %s: %w%s", selection.Version, err, diagnostic.suffix())
	}
	if err := checkSelection(selection); err != nil {
		return "", err
	}
	if err := verifyBinary(temporary, selection); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, binary); err != nil {
		return "", err
	}
	return binary, nil
}

func checkSelection(selection *Selection) error {
	current, err := Resolve(selection.Worktree)
	if err != nil {
		return err
	}
	if current == nil || current.Identity != selection.Identity {
		return errors.New("project Devflow selection changed during preparation; run devflow again")
	}
	return nil
}

// BuildEnv isolates launcher and adapter builds from application workspace,
// modfile and cross-compilation settings while preserving Go proxy/checksum policy.
func BuildEnv(env []string) []string {
	filtered := make([]string, 0, len(env)+4)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "GOWORK", "GOFLAGS", "GOOS", "GOARCH":
			continue
		}
		filtered = append(filtered, entry)
	}
	// Ambient workspace/modfile flags must not override the pin, and a launcher
	// must target this host even when its caller is cross-compiling application code.
	return append(filtered, "GOWORK=off", "GOFLAGS=", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
}

func verifyBinary(path string, selection *Selection) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read project runtime version: %w", err)
	}
	if info.Path != ModulePath+"/cmd/devflow" {
		return fmt.Errorf("project runtime entrypoint is %s; expected %s/cmd/devflow", info.Path, ModulePath)
	}
	return verifyModule(&info.Main, selection)
}

// VerifyAdapter checks the linked module, since Go can raise a direct requirement
// when another adapter dependency needs a newer Devflow release.
func VerifyAdapter(path string, selection *Selection) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read adapter Devflow version: %w", err)
	}
	if info.Main.Path == ModulePath {
		return verifyModule(&info.Main, selection)
	}
	for _, dependency := range info.Deps {
		if dependency.Path == ModulePath {
			return verifyModule(dependency, selection)
		}
	}
	return errors.New("compiled adapter does not contain the selected Devflow module")
}

func verifyModule(module *debug.Module, selection *Selection) error {
	if module.Path != ModulePath || module.Version != selection.Version {
		return fmt.Errorf("compiled Devflow module is %s %s; selected %s %s", module.Path, module.Version, ModulePath, selection.Version)
	}
	replacement := module.Replace
	replacementVersion := selection.ReplacementVersion
	replacementMatches := replacement != nil && replacement.Path == selection.ReplacementPath
	if selection.SourceRoot != "" {
		replacementVersion = "(devel)"
		if replacement != nil {
			// Go or Windows can canonicalize equivalent path spellings. Compare
			// the actual local module directory instead of treating aliases as versions.
			actual, actualErr := os.Stat(replacement.Path)
			expected, expectedErr := os.Stat(selection.SourceRoot)
			replacementMatches = actualErr == nil && expectedErr == nil && os.SameFile(actual, expected)
		}
	}
	if selection.ReplacementPath == "" {
		if replacement != nil {
			return fmt.Errorf("project runtime has an unexpected replacement %s %s", replacement.Path, replacement.Version)
		}
	} else if !replacementMatches || replacement.Version != replacementVersion {
		return fmt.Errorf("project runtime replacement %+v does not match selected %s %s", replacement, selection.ReplacementPath, selection.ReplacementVersion)
	}
	return nil
}

const diagnosticLimit = 16 * 1024

type tailBuffer struct{ data []byte }

func (b *tailBuffer) Write(data []byte) (int, error) {
	n := len(data)
	if n >= diagnosticLimit {
		b.data = append(b.data[:0], data[n-diagnosticLimit:]...)
		return n, nil
	}
	if excess := len(b.data) + n - diagnosticLimit; excess > 0 {
		copy(b.data, b.data[excess:])
		b.data = b.data[:len(b.data)-excess]
	}
	b.data = append(b.data, data...)
	return n, nil
}

func (b *tailBuffer) suffix() string {
	if text := strings.TrimSpace(string(b.data)); text != "" {
		return "\n" + text
	}
	return ""
}
