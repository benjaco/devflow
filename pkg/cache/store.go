package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/internal/jsonutil"
	"github.com/benjaco/devflow/pkg/project"
)

type Manifest struct {
	Version       int     `json:"version"`
	Task          string  `json:"task"`
	Key           string  `json:"key"`
	CreatedAt     string  `json:"createdAt"`
	Outputs       Outputs `json:"outputs"`
	InputsSummary Summary `json:"inputsSummary"`
}

type Outputs struct {
	Files []string `json:"files"`
	Dirs  []string `json:"dirs"`
}

type Summary struct {
	FileCount int      `json:"fileCount"`
	DirCount  int      `json:"dirCount"`
	Env       []string `json:"env"`
}

type Store struct {
	Root      string
	Namespace string
}

type EntrySummary struct {
	Namespace string `json:"namespace,omitempty"`
	Task      string `json:"task"`
	Key       string `json:"key"`
	CreatedAt string `json:"createdAt"`
}

func New(root string) *Store {
	return &Store{Root: root}
}

func NewNamespaced(root, namespace string) *Store {
	return &Store{Root: root, Namespace: sanitizeNamespace(namespace)}
}

func (s *Store) entriesRoot() string {
	root := filepath.Join(s.Root, "entries")
	if s.Namespace != "" {
		root = filepath.Join(root, s.Namespace)
	}
	return root
}

func (s *Store) EntriesRoot() string {
	return s.entriesRoot()
}

func (s *Store) EntryDir(task, key string) string {
	return filepath.Join(s.entriesRoot(), task, key)
}

func (s *Store) manifestPath(task, key string) string {
	return filepath.Join(s.EntryDir(task, key), "manifest.json")
}

func (s *Store) Load(task, key string) (*Manifest, bool, error) {
	if err := validateEntryNames(task, key); err != nil {
		return nil, false, err
	}
	path := s.manifestPath(task, key)
	if err := validateParents(s.Root, path); err != nil {
		return nil, false, err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, false, nil
	} else if err != nil && !os.IsNotExist(err) {
		return nil, false, err
	}
	var manifest Manifest
	if err := jsonutil.ReadFile(path, &manifest); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		var syntaxError *json.SyntaxError
		var typeError *json.UnmarshalTypeError
		if !errors.As(err, &syntaxError) && !errors.As(err, &typeError) {
			return nil, false, err
		}
		_ = fsutil.RemoveAllWritable(s.EntryDir(task, key))
		return nil, false, nil
	}
	if manifest.Version != 1 || manifest.Task != task || manifest.Key != key {
		return nil, false, nil
	}
	if err := validateOutputs(manifest.Outputs); err != nil {
		return nil, false, nil
	}
	return &manifest, true, nil
}

func (s *Store) Snapshot(worktree string, task project.Task, key string) (*Manifest, error) {
	return s.SnapshotContext(context.Background(), worktree, task, key, nil)
}

func (s *Store) SnapshotContext(ctx context.Context, worktree string, task project.Task, key string, onProgress func(fsutil.CopyProgress)) (*Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateEntryNames(task.Name, key); err != nil {
		return nil, err
	}
	files, dirs, err := classifyOutputs(worktree, task.Outputs)
	if err != nil {
		return nil, err
	}
	entryDir := s.EntryDir(task.Name, key)
	if manifest, ok, err := s.Load(task.Name, key); err != nil {
		return nil, err
	} else if ok {
		return manifest, nil
	}
	if err := os.MkdirAll(filepath.Dir(entryDir), 0o755); err != nil {
		return nil, err
	}
	tmpEntryDir, err := os.MkdirTemp(filepath.Dir(entryDir), filepath.Base(entryDir)+".tmp-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = fsutil.RemoveAllWritable(tmpEntryDir) }()
	if err := os.MkdirAll(filepath.Join(tmpEntryDir, "files"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(tmpEntryDir, "dirs"), 0o755); err != nil {
		return nil, err
	}

	sort.Strings(files)
	sort.Strings(dirs)

	copier := fsutil.NewCopier(fsutil.CopyOptions{OnProgress: onProgress})
	for i, rel := range files {
		src := filepath.Join(worktree, rel)
		info, err := os.Lstat(src)
		if err != nil {
			return nil, fmt.Errorf("declared output file %q missing: %w", rel, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("declared output file %q is not a regular file", rel)
		}
		if err := copier.Copy(ctx, filepath.Dir(src), src, filepath.Join(tmpEntryDir, "files", strconv.Itoa(i))); err != nil {
			return nil, err
		}
	}
	for i, rel := range dirs {
		src := filepath.Join(worktree, rel)
		if info, err := os.Lstat(src); err != nil {
			return nil, fmt.Errorf("declared output dir %q missing: %w", rel, err)
		} else if !info.IsDir() {
			return nil, fmt.Errorf("declared output dir %q is not a directory", rel)
		}
		if err := copier.Copy(ctx, src, src, filepath.Join(tmpEntryDir, "dirs", strconv.Itoa(i))); err != nil {
			return nil, err
		}
	}

	manifest := &Manifest{
		Version:   1,
		Task:      task.Name,
		Key:       key,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Outputs: Outputs{
			Files: files,
			Dirs:  dirs,
		},
		InputsSummary: Summary{
			FileCount: len(task.Inputs.Paths) + len(task.Inputs.Files) + len(task.Inputs.Globs),
			DirCount:  len(task.Inputs.Dirs),
			Env:       append([]string(nil), task.Inputs.Env...),
		},
	}
	if err := jsonutil.WriteFileAtomic(filepath.Join(tmpEntryDir, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpEntryDir, entryDir); err != nil {
		if existing, ok, loadErr := s.Load(task.Name, key); loadErr == nil && ok {
			return existing, nil
		}
		_ = fsutil.RemoveAllWritable(entryDir)
		if retryErr := os.Rename(tmpEntryDir, entryDir); retryErr != nil {
			return nil, err
		}
	}
	return manifest, nil
}

func classifyOutputs(worktree string, outputs project.Outputs) ([]string, []string, error) {
	files := append([]string(nil), outputs.Files...)
	dirs := append([]string(nil), outputs.Dirs...)
	all := append(append(append([]string(nil), files...), dirs...), outputs.Paths...)
	for _, rel := range all {
		if err := validateOutputPath(rel); err != nil {
			return nil, nil, err
		}
		if err := validateParents(worktree, filepath.Join(worktree, rel)); err != nil {
			return nil, nil, err
		}
	}
	for _, rel := range outputs.Paths {
		info, err := os.Lstat(filepath.Join(worktree, rel))
		if err != nil {
			return nil, nil, fmt.Errorf("declared output path %q missing: %w", rel, err)
		}
		if info.IsDir() {
			dirs = append(dirs, rel)
		} else {
			files = append(files, rel)
		}
	}
	declared := Outputs{Files: files, Dirs: dirs}
	if err := validateOutputs(declared); err != nil {
		return nil, nil, err
	}
	// Validate every declaration before removing redundant children. A wrong
	// file/dir declaration must not be hidden by a containing directory.
	for _, output := range outputArtifacts(declared, "", false) {
		if err := validateArtifact(filepath.Join(worktree, output.rel), output.directory); err != nil {
			return nil, nil, fmt.Errorf("declared output %q: %w", output.rel, err)
		}
	}
	files, dirs = nil, nil
	for _, output := range outputArtifacts(declared, "", true) {
		if output.directory {
			dirs = append(dirs, output.rel)
		} else {
			files = append(files, output.rel)
		}
	}
	return files, dirs, nil
}

func (s *Store) Restore(worktree string, taskName, key string) (bool, error) {
	return s.RestoreContext(context.Background(), worktree, taskName, key, nil)
}

func (s *Store) RestoreContext(ctx context.Context, worktree string, taskName, key string, onProgress func(fsutil.CopyProgress)) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	manifest, ok, err := s.Load(taskName, key)
	if err != nil || !ok {
		return ok, err
	}
	entryDir := s.EntryDir(taskName, key)

	return restoreOutputs(ctx, worktree, entryDir, manifest.Outputs, onProgress)
}

func (s *Store) List() ([]EntrySummary, error) {
	root := s.entriesRoot()
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	entries := make([]EntrySummary, 0)
	taskDirs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, taskDir := range taskDirs {
		if !taskDir.IsDir() {
			continue
		}
		keyDirs, err := os.ReadDir(filepath.Join(root, taskDir.Name()))
		if err != nil {
			return nil, err
		}
		for _, keyDir := range keyDirs {
			if !keyDir.IsDir() {
				continue
			}
			manifest, ok, err := s.Load(taskDir.Name(), keyDir.Name())
			if err != nil || !ok {
				continue
			}
			entries = append(entries, EntrySummary{
				Namespace: s.Namespace,
				Task:      manifest.Task,
				Key:       manifest.Key,
				CreatedAt: manifest.CreatedAt,
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Task != entries[j].Task {
			return entries[i].Task < entries[j].Task
		}
		return entries[i].Key < entries[j].Key
	})
	return entries, nil
}

func (s *Store) Invalidate(task string) error {
	path := s.entriesRoot()
	if task != "" {
		if err := validateComponent("task", task); err != nil {
			return err
		}
		path = filepath.Join(path, task)
	}
	if err := validateParents(s.Root, path); err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return fsutil.RemoveAllWritable(path)
}

func (s *Store) GC(keepPerTask int) (int, error) {
	if keepPerTask <= 0 {
		keepPerTask = 1
	}
	root := s.entriesRoot()
	taskDirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, taskDir := range taskDirs {
		if !taskDir.IsDir() {
			continue
		}
		taskName := taskDir.Name()
		items, err := s.List()
		if err != nil {
			return removed, err
		}
		filtered := make([]EntrySummary, 0)
		for _, item := range items {
			if item.Task == taskName {
				filtered = append(filtered, item)
			}
		}
		sort.Slice(filtered, func(i, j int) bool {
			return filtered[i].CreatedAt > filtered[j].CreatedAt
		})
		for i := keepPerTask; i < len(filtered); i++ {
			if err := fsutil.RemoveAllWritable(s.EntryDir(filtered[i].Task, filtered[i].Key)); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

func sanitizeNamespace(namespace string) string {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range namespace {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return "default"
	}
	return out
}
