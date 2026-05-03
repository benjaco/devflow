package fingerprint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/benjaco/devflow/internal/pathspec"
	"github.com/benjaco/devflow/pkg/project"
)

const EngineKeyVersion = "devflow-v1"

type TaskKeyInput struct {
	Task               project.Task
	DepKeys            []string
	InputHashes        []string
	EnvValues          []string
	CustomFingerprints []string
	Override           string
}

func HashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func HashDir(root string, ignore []string) (string, error) {
	return hashDir(root, "", ignore)
}

func HashInputDir(root, inputDir string, ignore []string) (string, error) {
	return hashDir(root, inputDir, ignore)
}

func hashDir(root, inputDir string, ignore []string) (string, error) {
	entries := make([]string, 0)
	inputDir = cleanInputPath(inputDir)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		if ignored(rel, inputDir, ignore) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			entries = append(entries, "dir:"+rel)
			return nil
		}
		sum, err := HashFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, "file:"+rel+":"+info.Mode().String()+":"+sum)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(entries)
	return hashStrings(entries), nil
}

func HashEnv(env map[string]string, keys []string) []string {
	out := make([]string, 0, len(keys))
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	for _, key := range sorted {
		out = append(out, key+"="+env[key])
	}
	return out
}

func CollectTaskInputs(ctx context.Context, worktree string, task project.Task, rt *project.Runtime) (hashes []string, envValues []string, custom []string, err error) {
	fileSet := map[string]bool{}
	for _, inputPath := range task.Inputs.Paths {
		if ignored(cleanInputPath(inputPath), "", task.Inputs.Ignore) {
			continue
		}
		if collectErr := collectPathInput(worktree, inputPath, task.Inputs.Ignore, fileSet); collectErr != nil {
			err = collectErr
			return
		}
	}
	for _, file := range task.Inputs.Files {
		if ignored(cleanInputPath(file), "", task.Inputs.Ignore) {
			continue
		}
		path := filepath.Join(worktree, file)
		sum, hashErr := HashFile(path)
		if hashErr != nil {
			if os.IsNotExist(hashErr) {
				sum = "missing"
			} else {
				err = hashErr
				return
			}
		}
		fileSet["file:"+filepath.ToSlash(file)+":"+sum] = true
	}
	for _, dir := range task.Inputs.Dirs {
		path := filepath.Join(worktree, dir)
		sum, hashErr := HashInputDir(path, dir, task.Inputs.Ignore)
		if hashErr != nil {
			if os.IsNotExist(hashErr) {
				sum = "missing"
			} else {
				err = hashErr
				return
			}
		}
		fileSet["dir:"+filepath.ToSlash(dir)+":"+sum] = true
	}
	for _, pattern := range task.Inputs.Globs {
		matches, globErr := pathspec.ExpandGlob(worktree, pattern)
		if globErr != nil {
			err = globErr
			return
		}
		matched := false
		for _, rel := range matches {
			if ignored(cleanInputPath(rel), "", task.Inputs.Ignore) {
				continue
			}
			matched = true
			path := filepath.Join(worktree, rel)
			sum, hashErr := HashFile(path)
			if hashErr != nil {
				err = hashErr
				return
			}
			fileSet["glob:"+filepath.ToSlash(pattern)+":"+filepath.ToSlash(rel)+":"+sum] = true
		}
		if !matched {
			fileSet["glob:"+filepath.ToSlash(pattern)+":missing"] = true
		}
	}
	for item := range fileSet {
		hashes = append(hashes, item)
	}
	sort.Strings(hashes)
	envValues = HashEnv(rt.Env, task.Inputs.Env)
	for _, fn := range task.Inputs.Custom {
		value, fnErr := fn(ctx, rt)
		if fnErr != nil {
			err = fnErr
			return
		}
		custom = append(custom, value)
	}
	sort.Strings(custom)
	return
}

func TaskSignature(task project.Task) (string, error) {
	payload := struct {
		Name                      string                `json:"name"`
		Kind                      project.Kind          `json:"kind"`
		Deps                      []string              `json:"deps"`
		RequiredCLIs              []string              `json:"requiredCLIs"`
		Inputs                    project.Inputs        `json:"inputs"`
		Outputs                   project.Outputs       `json:"outputs"`
		Cache                     bool                  `json:"cache"`
		Restart                   project.RestartPolicy `json:"restart"`
		WatchRestartOnServiceDeps bool                  `json:"watchRestartOnServiceDeps"`
		AllowInWatch              bool                  `json:"allowInWatch"`
		Tags                      []string              `json:"tags"`
		Description               string                `json:"description"`
		Signature                 string                `json:"signature"`
	}{
		Name:                      task.Name,
		Kind:                      task.Kind,
		Deps:                      append([]string(nil), task.Deps...),
		RequiredCLIs:              append([]string(nil), task.RequiredCLIs...),
		Inputs:                    task.Inputs,
		Outputs:                   task.Outputs,
		Cache:                     task.Cache,
		Restart:                   task.Restart,
		WatchRestartOnServiceDeps: task.WatchRestartOnServiceDeps,
		AllowInWatch:              task.AllowInWatch,
		Tags:                      append([]string(nil), task.Tags...),
		Description:               task.Description,
		Signature:                 task.Signature,
	}
	sort.Strings(payload.Deps)
	sort.Strings(payload.RequiredCLIs)
	sort.Strings(payload.Tags)
	sort.Strings(payload.Inputs.Paths)
	sort.Strings(payload.Inputs.Files)
	sort.Strings(payload.Inputs.Dirs)
	sort.Strings(payload.Inputs.Globs)
	sort.Strings(payload.Inputs.Env)
	sort.Strings(payload.Inputs.Ignore)
	sort.Strings(payload.Outputs.Paths)
	sort.Strings(payload.Outputs.Files)
	sort.Strings(payload.Outputs.Dirs)
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func TaskKey(in TaskKeyInput) (string, error) {
	if in.Task.CacheKeyOverride != nil {
		if strings.TrimSpace(in.Override) == "" {
			return "", fmt.Errorf("task %q cache key override returned empty value", in.Task.Name)
		}
		return OverrideTaskKey(in.Task.Name, in.Override), nil
	}
	sig, err := TaskSignature(in.Task)
	if err != nil {
		return "", err
	}
	parts := []string{
		EngineKeyVersion,
		in.Task.Name,
		sig,
	}
	parts = append(parts, cloneSorted(in.DepKeys)...)
	parts = append(parts, cloneSorted(in.InputHashes)...)
	parts = append(parts, cloneSorted(in.EnvValues)...)
	parts = append(parts, cloneSorted(in.CustomFingerprints)...)
	return hashStrings(parts), nil
}

func OverrideTaskKey(taskName, override string) string {
	return hashStrings([]string{EngineKeyVersion, taskName, override})
}

func collectPathInput(worktree, rel string, ignore []string, fileSet map[string]bool) error {
	rel = cleanInputPath(rel)
	pathValue := filepath.Join(worktree, rel)
	info, err := os.Stat(pathValue)
	if err != nil {
		if os.IsNotExist(err) {
			fileSet["path:"+filepath.ToSlash(rel)+":missing"] = true
			return nil
		}
		return err
	}
	if info.IsDir() {
		sum, err := HashInputDir(pathValue, rel, ignore)
		if err != nil {
			return err
		}
		fileSet["path-dir:"+filepath.ToSlash(rel)+":"+sum] = true
		return nil
	}
	sum, err := HashFile(pathValue)
	if err != nil {
		return err
	}
	fileSet["path-file:"+filepath.ToSlash(rel)+":"+sum] = true
	return nil
}

func ignored(pathValue, inputDir string, ignore []string) bool {
	for _, pattern := range ignore {
		if matchInputIgnore(pattern, pathValue) {
			return true
		}
		if inputDir != "" {
			rootRelative := pathValue
			if rootRelative == "" {
				rootRelative = inputDir
			} else {
				rootRelative = inputDir + "/" + rootRelative
			}
			if matchInputIgnore(pattern, rootRelative) {
				return true
			}
		}
	}
	return false
}

func matchInputIgnore(pattern, candidate string) bool {
	pattern = cleanInputPath(pattern)
	candidate = cleanInputPath(candidate)
	if ok, _ := path.Match(pattern, candidate); ok {
		return true
	}
	return candidate == pattern || strings.HasPrefix(candidate, pattern+"/")
}

func cleanInputPath(value string) string {
	value = filepath.ToSlash(filepath.Clean(value))
	if value == "." {
		return ""
	}
	return value
}

func hashStrings(items []string) string {
	h := sha256.New()
	for _, item := range items {
		_, _ = io.WriteString(h, item)
		_, _ = io.WriteString(h, "\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func cloneSorted(items []string) []string {
	out := append([]string(nil), items...)
	sort.Strings(out)
	return out
}
