package pathspec

import (
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
	pattern = Clean(pattern)
	root = filepath.Clean(root)
	matches := make([]string, 0)
	err := filepath.WalkDir(root, func(full string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
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
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return matches, nil
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
