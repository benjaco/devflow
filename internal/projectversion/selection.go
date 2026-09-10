package projectversion

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

const ModulePath = "github.com/benjaco/devflow"

type Selection struct {
	Worktree           string
	Version            string
	ReplacementPath    string
	ReplacementVersion string
	SourceRoot         string
	Identity           string
	modBytes           []byte
	sumBytes           []byte
}

// Resolve reads the project's explicit requirement, independently of go.work or
// unrelated dependencies. No requirement leaves the installed launcher in charge.
func Resolve(worktree string) (*Selection, error) {
	root, err := filepath.Abs(worktree)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, "go.mod")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	file, err := modfile.Parse(path, data, nil)
	if err != nil {
		return nil, err
	}
	var selected *Selection
	for _, require := range file.Require {
		if require.Mod.Path != ModulePath {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("%s: multiple requirements for %s", path, ModulePath)
		}
		if err := exactVersion(require.Mod.Path, require.Mod.Version); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		selected = &Selection{Worktree: root, Version: require.Mod.Version, modBytes: data}
	}
	if selected == nil {
		return nil, nil
	}
	// Instance discovery already canonicalizes the worktree. The launcher must
	// share that identity when cwd reaches the same checkout through an alias.
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	selected.Worktree = root
	for _, excluded := range file.Exclude {
		if excluded.Mod.Path == ModulePath && excluded.Mod.Version == selected.Version {
			return nil, fmt.Errorf("%s: selected Devflow version %s is excluded", path, selected.Version)
		}
	}
	var replacement *modfile.Replace
	matching := make(map[string]module.Version)
	for _, replace := range file.Replace {
		if replace.Old.Path != ModulePath || (replace.Old.Version != "" && replace.Old.Version != selected.Version) {
			continue
		}
		if previous, exists := matching[replace.Old.Version]; exists && previous != replace.New {
			return nil, fmt.Errorf("%s: conflicting replacements for %s %s", path, ModulePath, replace.Old.Version)
		}
		matching[replace.Old.Version] = replace.New
		if replacement == nil || replace.Old.Version != "" {
			replacement = replace
		}
	}
	if replacement != nil {
		selected.ReplacementPath = replacement.New.Path
		selected.ReplacementVersion = replacement.New.Version
		if replacement.New.Version == "" {
			if !filepath.IsAbs(selected.ReplacementPath) {
				selected.ReplacementPath = filepath.Join(root, selected.ReplacementPath)
			}
			selected.SourceRoot, err = filepath.EvalSymlinks(selected.ReplacementPath)
			if err != nil {
				return nil, fmt.Errorf("local Devflow replacement: %w", err)
			}
			selected.ReplacementPath = selected.SourceRoot
		} else if err := exactVersion(replacement.New.Path, replacement.New.Version); err != nil {
			return nil, fmt.Errorf("%s: replacement: %w", path, err)
		}
	}
	selected.sumBytes, err = os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x00%s\x00%s\x00%s\x00", runtime.GOOS, runtime.GOARCH, root, selected.SourceRoot)
	_, _ = hash.Write(data)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(selected.sumBytes)
	if selected.SourceRoot != "" {
		if err := hashSource(hash, selected.SourceRoot); err != nil {
			return nil, fmt.Errorf("local Devflow replacement: %w", err)
		}
	}
	selected.Identity = hex.EncodeToString(hash.Sum(nil))
	return selected, nil
}

func exactVersion(path, version string) error {
	if err := module.Check(path, version); err != nil {
		return err
	}
	if module.CanonicalVersion(version) != version {
		return fmt.Errorf("%s requires a canonical version, got %q", path, version)
	}
	return nil
}

// ModuleSource reconstructs only the chosen Devflow dependency. Application
// replacements and toolchain settings must not alter the launcher's dependency graph.
func (s *Selection) ModuleSource(modulePath string) ([]byte, error) {
	file := new(modfile.File)
	if err := file.AddModuleStmt(modulePath); err != nil {
		return nil, err
	}
	if err := file.AddGoStmt("1.27.1"); err != nil {
		return nil, err
	}
	if err := file.AddRequire(ModulePath, s.Version); err != nil {
		return nil, err
	}
	if s.ReplacementPath != "" {
		if err := file.AddReplace(ModulePath, s.Version, s.ReplacementPath, s.ReplacementVersion); err != nil {
			return nil, err
		}
	}
	return file.Format()
}

func (s *Selection) SumBytes() []byte {
	return append([]byte(nil), s.sumBytes...)
}

func BinaryPath(selection *Selection) string {
	name := "devflow"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(selection.Worktree, ".devflow", "versions", selection.Identity, name)
}

func hashSource(hash io.Writer, root string) error {
	if err := hashSourceFile(hash, root, filepath.Join(root, "go.mod")); err != nil {
		return err
	}
	if err := hashSourceFile(hash, root, filepath.Join(root, "go.sum")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Include non-Go assets too: Devflow embeds user docs and database images.
	// Tests and their fixtures do not contribute to the selected command binary.
	for _, dir := range []string{"cmd", "internal", "pkg", "docs_users"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if path == base && errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, "_test.go") || !entry.Type().IsRegular() {
				return nil
			}
			return hashSourceFile(hash, root, path)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func hashSourceFile(hash io.Writer, root, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	label, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	fmt.Fprintf(hash, "\x00%s\x00", filepath.ToSlash(label))
	_, err = io.Copy(hash, file)
	return err
}
