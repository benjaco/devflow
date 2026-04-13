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
	"path/filepath"
	"sort"
	"strings"

	"devflow/pkg/project"
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
	entries := make([]string, 0)
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
		if ignored(rel, ignore) {
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
	for _, file := range task.Inputs.Files {
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
		sum, hashErr := HashDir(path, task.Inputs.Ignore)
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
	sort.Strings(payload.Tags)
	sort.Strings(payload.Inputs.Files)
	sort.Strings(payload.Inputs.Dirs)
	sort.Strings(payload.Inputs.Env)
	sort.Strings(payload.Inputs.Ignore)
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

func ignored(path string, ignore []string) bool {
	for _, pattern := range ignore {
		if ok, _ := filepath.Match(pattern, path); ok {
			return true
		}
		if strings.HasPrefix(path, pattern+"/") {
			return true
		}
	}
	return false
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
