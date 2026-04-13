package project

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	return LoadOptionalDotEnv(filepath.Join(worktree, rel))
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
