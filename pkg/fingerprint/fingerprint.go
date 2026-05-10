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
	"sync"

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

type FilteredContentCache struct {
	mu      sync.Mutex
	entries map[filteredContentCacheKey]filteredContentCacheEntry
}

type filteredContentCacheKey struct {
	Path      string
	Size      int64
	ModTimeNS int64
	Signature string
}

type filteredContentCacheEntry struct {
	Sum   string
	Empty bool
}

func NewFilteredContentCache() *FilteredContentCache {
	return &FilteredContentCache{entries: map[filteredContentCacheKey]filteredContentCacheEntry{}}
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

func HashBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
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
	return CollectTaskInputsWithCache(ctx, worktree, task, rt, nil)
}

func CollectTaskInputsWithCache(ctx context.Context, worktree string, task project.Task, rt *project.Runtime, filteredCache *FilteredContentCache) (hashes []string, envValues []string, custom []string, err error) {
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
	for _, input := range task.Inputs.Filtered {
		if collectErr := collectFilteredInput(ctx, worktree, input, task.Inputs.Ignore, rt, filteredCache, fileSet); collectErr != nil {
			err = collectErr
			return
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
		Stamp                     bool                  `json:"stamp"`
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
		Stamp:                     task.Stamp,
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
	sort.Slice(payload.Inputs.Filtered, func(i, j int) bool {
		if payload.Inputs.Filtered[i].Path != payload.Inputs.Filtered[j].Path {
			return payload.Inputs.Filtered[i].Path < payload.Inputs.Filtered[j].Path
		}
		return payload.Inputs.Filtered[i].Filter.Signature < payload.Inputs.Filtered[j].Filter.Signature
	})
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

func collectFilteredInput(ctx context.Context, worktree string, input project.FilteredInput, ignore []string, rt *project.Runtime, filteredCache *FilteredContentCache, fileSet map[string]bool) error {
	inputPath := cleanInputPath(input.Path)
	if inputPath == "" || ignored(inputPath, "", ignore) {
		return nil
	}
	signature := strings.TrimSpace(input.Filter.Signature)
	if signature == "" {
		signature = "identity"
	}
	if pathspec.HasGlob(inputPath) {
		matches, err := pathspec.ExpandGlob(worktree, inputPath)
		if err != nil {
			return err
		}
		for _, rel := range matches {
			if ignored(cleanInputPath(rel), "", ignore) {
				continue
			}
			if err := collectFilteredFile(ctx, worktree, inputPath, rel, signature, input.Filter, rt, filteredCache, fileSet); err != nil {
				return err
			}
		}
		return nil
	}

	full := filepath.Join(worktree, inputPath)
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return collectFilteredFile(ctx, worktree, inputPath, inputPath, signature, input.Filter, rt, filteredCache, fileSet)
	}
	return filepath.WalkDir(full, func(pathValue string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(worktree, pathValue)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		dirRel := strings.TrimPrefix(rel, inputPath+"/")
		if ignored(dirRel, inputPath, ignore) {
			return nil
		}
		return collectFilteredFile(ctx, worktree, inputPath, rel, signature, input.Filter, rt, filteredCache, fileSet)
	})
}

func collectFilteredFile(ctx context.Context, worktree, inputPath, rel, signature string, filter project.FileContentFilter, rt *project.Runtime, filteredCache *FilteredContentCache, fileSet map[string]bool) error {
	fullPath := filepath.Join(worktree, filepath.FromSlash(rel))
	info, err := os.Stat(fullPath)
	if err != nil {
		return err
	}
	if entry, ok := filteredCache.get(fullPath, info, signature); ok {
		if entry.Empty {
			return nil
		}
		fileSet["filtered:"+filepath.ToSlash(inputPath)+":"+signature+":"+filepath.ToSlash(rel)+":"+entry.Sum] = true
		return nil
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return err
	}
	filtered, err := filter.Apply(ctx, rt, project.FileContent{Path: rel, Content: data})
	if err != nil {
		return err
	}
	empty := len(strings.TrimSpace(string(filtered))) == 0
	sum := ""
	if !empty {
		sum = HashBytes(filtered)
	}
	filteredCache.set(fullPath, info, signature, filteredContentCacheEntry{Sum: sum, Empty: empty})
	if empty {
		return nil
	}
	fileSet["filtered:"+filepath.ToSlash(inputPath)+":"+signature+":"+filepath.ToSlash(rel)+":"+sum] = true
	return nil
}

func (c *FilteredContentCache) get(path string, info os.FileInfo, signature string) (filteredContentCacheEntry, bool) {
	if c == nil {
		return filteredContentCacheEntry{}, false
	}
	key := filteredContentCacheKey{
		Path:      filepath.Clean(path),
		Size:      info.Size(),
		ModTimeNS: info.ModTime().UnixNano(),
		Signature: signature,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	return entry, ok
}

func (c *FilteredContentCache) set(path string, info os.FileInfo, signature string, entry filteredContentCacheEntry) {
	if c == nil {
		return
	}
	clean := filepath.Clean(path)
	key := filteredContentCacheKey{
		Path:      clean,
		Size:      info.Size(),
		ModTimeNS: info.ModTime().UnixNano(),
		Signature: signature,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for existing := range c.entries {
		if existing.Path == clean && existing.Signature == signature {
			delete(c.entries, existing)
		}
	}
	c.entries[key] = entry
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
