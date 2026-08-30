package pathspec

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func Clean(value string) string {
	value = filepath.ToSlash(filepath.Clean(value))
	if value == "." {
		return ""
	}
	return value
}

func HasGlob(value string) bool {
	return strings.ContainsAny(value, "*?[")
}

func MatchGlob(pattern, candidate string) bool {
	pattern = Clean(pattern)
	candidate = Clean(candidate)
	if pattern == "" {
		return candidate == ""
	}
	return matchSegments(split(pattern), split(candidate))
}

func ExpandGlob(root, pattern string) ([]string, error) {
	return expandGlob(root, pattern, filepath.WalkDir)
}

type walkDirFunc func(string, fs.WalkDirFunc) error

func expandGlob(root, pattern string, walkDir walkDirFunc) ([]string, error) {
	var err error
	pattern, err = cleanRelativeGlob(pattern)
	if err != nil {
		return nil, err
	}
	root = filepath.Clean(root)
	scanRoot, err := globScanRoot(root, pattern)
	if err != nil {
		return nil, err
	}
	prefixAvailable, err := globPrefixAvailable(root, scanRoot)
	if err != nil {
		return nil, err
	}
	if !prefixAvailable {
		return []string{}, nil
	}

	matches := make([]string, 0)
	scanRootVisited := false
	err = walkDir(scanRoot, func(full string, entry os.DirEntry, err error) error {
		if err != nil {
			if samePath(full, scanRoot) && !scanRootVisited && os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !scanRootVisited && samePath(full, scanRoot) {
			scanRootVisited = true
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		rel = Clean(rel)
		if MatchGlob(pattern, rel) {
			matches = append(matches, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return matches, nil
}

func cleanRelativeGlob(pattern string) (string, error) {
	portable := strings.ReplaceAll(pattern, `\`, "/")
	if strings.HasPrefix(portable, "/") || portableVolumeName(portable) != "" {
		return "", fmt.Errorf("glob pattern %q must be worktree-relative", pattern)
	}
	for _, segment := range strings.Split(portable, "/") {
		if segment == ".." {
			return "", fmt.Errorf("glob pattern %q must not contain a parent path segment", pattern)
		}
	}

	cleaned := Clean(pattern)
	native := filepath.FromSlash(cleaned)
	if path.IsAbs(cleaned) || filepath.IsAbs(native) || filepath.VolumeName(native) != "" {
		return "", fmt.Errorf("glob pattern %q must be worktree-relative", pattern)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("glob pattern %q escapes the worktree", pattern)
	}
	return cleaned, nil
}

func portableVolumeName(value string) string {
	if len(value) >= 2 && value[1] == ':' && isASCIILetter(value[0]) {
		return value[:2]
	}
	return ""
}

func isASCIILetter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func globScanRoot(root, pattern string) (string, error) {
	prefix := make([]string, 0)
	for _, segment := range split(pattern) {
		if HasGlob(segment) {
			break
		}
		prefix = append(prefix, segment)
	}

	scanRoot := root
	if len(prefix) > 0 {
		scanRoot = filepath.Join(root, filepath.FromSlash(strings.Join(prefix, "/")))
	}
	rel, err := filepath.Rel(root, scanRoot)
	if err != nil {
		return "", fmt.Errorf("resolve glob scan root: %w", err)
	}
	if relativePathEscapes(rel) {
		return "", fmt.Errorf("glob scan root %q escapes worktree root %q", scanRoot, root)
	}
	return scanRoot, nil
}

// globPrefixAvailable checks only ancestors of scanRoot. Walking a deep path
// through a directory symlink would follow that intermediate symlink, unlike a
// WalkDir rooted at the worktree, which encounters and does not follow it.
func globPrefixAvailable(root, scanRoot string) (bool, error) {
	rel, err := filepath.Rel(root, scanRoot)
	if err != nil {
		return false, fmt.Errorf("resolve glob scan root: %w", err)
	}
	if relativePathEscapes(rel) {
		return false, fmt.Errorf("glob scan root %q escapes worktree root %q", scanRoot, root)
	}
	if rel == "." {
		return true, nil
	}

	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}

	current := root
	segments := strings.Split(rel, string(filepath.Separator))
	for _, segment := range segments[:len(segments)-1] {
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, nil
		}
	}
	return true, nil
}

func relativePathEscapes(rel string) bool {
	rel = filepath.Clean(rel)
	return filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if left == right {
		return true
	}
	leftRel, err := filepath.Rel(left, right)
	return err == nil && leftRel == "."
}

func split(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, "/")
}

func matchSegments(pattern, candidate []string) bool {
	if len(pattern) == 0 {
		return len(candidate) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(candidate); i++ {
			if matchSegments(pattern[1:], candidate[i:]) {
				return true
			}
		}
		return false
	}
	if len(candidate) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], candidate[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pattern[1:], candidate[1:])
}
