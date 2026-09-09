package project

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func LoadDotEnv(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	env := map[string]string{}
	scanner := bufio.NewScanner(file)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("parse %s:%d: missing '='", path, lineNo)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("parse %s:%d: empty key", path, lineNo)
		}
		env[key] = parseDotEnvValue(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return env, nil
}

func LoadOptionalDotEnv(path string) (map[string]string, error) {
	env, err := LoadDotEnv(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	return env, nil
}

func LoadOptionalDotEnvInWorktree(worktree, rel string) (map[string]string, error) {
	if filepath.IsAbs(rel) {
		return LoadOptionalDotEnv(rel)
	}
	path := filepath.Join(worktree, rel)
	env, err := LoadDotEnv(path)
	if err == nil || !os.IsNotExist(err) {
		return env, err
	}
	// An explicit local file, including a broken symlink, must not borrow defaults.
	if _, statErr := os.Lstat(path); statErr == nil {
		return nil, err
	}
	if fallback := mainWorktreeDotEnvPath(worktree, rel); fallback != "" {
		return LoadOptionalDotEnv(fallback)
	}
	return map[string]string{}, nil
}

func mainWorktreeDotEnvPath(worktree, rel string) string {
	// Git discovery is optional and local; a missing file must not hang startup.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	selected, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return ""
	}
	selected, err = filepath.Abs(selected)
	if err != nil {
		return ""
	}
	root := dotEnvGitRoot(ctx, selected)
	if root == "" {
		return ""
	}
	relative, err := filepath.Rel(root, filepath.Join(selected, rel))
	if err != nil || !filepath.IsLocal(relative) {
		return ""
	}
	output := dotEnvGitOutput(ctx, selected, "worktree", "list", "--porcelain", "-z")
	// Git lists the main checkout first. NUL records preserve quoted/newline paths.
	first, _, _ := strings.Cut(string(output), "\x00\x00")
	main := ""
	for _, field := range strings.Split(first, "\x00") {
		if field == "bare" {
			return ""
		}
		if value, ok := strings.CutPrefix(field, "worktree "); ok {
			main = value
		}
	}
	mainInfo, err := os.Stat(main)
	if err != nil || !mainInfo.IsDir() {
		return ""
	}
	rootInfo, err := os.Stat(root)
	if err != nil || os.SameFile(rootInfo, mainInfo) {
		return ""
	}
	// Some separate-git-dir layouts list metadata rather than an actual checkout.
	mainRootInfo, err := os.Stat(dotEnvGitRoot(ctx, main))
	if err != nil || !os.SameFile(mainRootInfo, mainInfo) {
		return ""
	}
	return filepath.Join(main, relative)
}

func dotEnvGitRoot(ctx context.Context, dir string) string {
	root := strings.TrimSuffix(string(dotEnvGitOutput(ctx, dir, "rev-parse", "--show-toplevel")), "\n")
	if !filepath.IsAbs(root) {
		return ""
	}
	return root
}

func dotEnvGitOutput(ctx context.Context, dir string, args ...string) []byte {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		// A Git hook's repository overrides must not redirect the selected worktree.
		switch strings.ToUpper(key) {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	return output
}

func MergeEnvMaps(layers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, layer := range layers {
		for key, value := range layer {
			out[key] = value
		}
	}
	return out
}

func parseDotEnvValue(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") && len(value) >= 2 {
		return strings.Trim(value[1:len(value)-1], " ")
	}
	if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") && len(value) >= 2 {
		return value[1 : len(value)-1]
	}
	if idx := strings.Index(value, " #"); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}
