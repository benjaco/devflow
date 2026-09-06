// Package adaptersource defines the filename scope shared by adapter bootstrap
// and change planning.
package adaptersource

import (
	"path/filepath"
	"strings"
)

// IsSource reports whether a worktree-relative path names a compiled adapter
// source. It does not inspect the filesystem, so removed paths remain classifiable.
func IsSource(relative string) bool {
	relative = filepath.ToSlash(relative)
	for strings.HasPrefix(relative, "./") {
		relative = strings.TrimPrefix(relative, "./")
	}
	// Do not clean traversal into a root filename or absorb nested Go packages.
	if strings.Contains(relative, "/") {
		return false
	}
	return relative == "devflow.project.go" ||
		(strings.HasPrefix(relative, "devflow_") && strings.HasSuffix(relative, ".go") && !strings.HasSuffix(relative, "_test.go"))
}
