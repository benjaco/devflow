package cache

import (
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

func (s *Store) EntryDir(task, key string) string {
	return filepath.Join(s.entriesRoot(), task, key)
}

func (s *Store) manifestPath(task, key string) string {
	return filepath.Join(s.EntryDir(task, key), "manifest.json")
}

func (s *Store) Load(task, key string) (*Manifest, bool, error) {
	path := s.manifestPath(task, key)
	var manifest Manifest
	if err := jsonutil.ReadFile(path, &manifest); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		_ = os.RemoveAll(s.EntryDir(task, key))
		return nil, false, nil
	}
	return &manifest, true, nil
}

func (s *Store) Snapshot(worktree string, task project.Task, key string) (*Manifest, error) {
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
	defer func() { _ = os.RemoveAll(tmpEntryDir) }()
	if err := os.MkdirAll(filepath.Join(tmpEntryDir, "files"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(tmpEntryDir, "dirs"), 0o755); err != nil {
		return nil, err
	}

	files := append([]string(nil), task.Outputs.Files...)
	dirs := append([]string(nil), task.Outputs.Dirs...)
	sort.Strings(files)
	sort.Strings(dirs)

	for i, rel := range files {
		src := filepath.Join(worktree, rel)
		if _, err := os.Stat(src); err != nil {
			return nil, fmt.Errorf("declared output file %q missing: %w", rel, err)
		}
		if err := fsutil.CopyFile(src, filepath.Join(tmpEntryDir, "files", strconv.Itoa(i))); err != nil {
			return nil, err
		}
	}
	for i, rel := range dirs {
		src := filepath.Join(worktree, rel)
		if _, err := os.Stat(src); err != nil {
			return nil, fmt.Errorf("declared output dir %q missing: %w", rel, err)
		}
		if err := fsutil.CopyDir(src, filepath.Join(tmpEntryDir, "dirs", strconv.Itoa(i))); err != nil {
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
			FileCount: len(task.Inputs.Files),
			DirCount:  len(task.Inputs.Dirs),
			Env:       append([]string(nil), task.Inputs.Env...),
		},
	}
	if err := jsonutil.WriteFileAtomic(filepath.Join(tmpEntryDir, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpEntryDir, entryDir); err != nil {
		if existing, ok, loadErr := s.Load(task.Name, key); loadErr == nil && ok {
			return existing, nil
		}
		_ = os.RemoveAll(entryDir)
		if retryErr := os.Rename(tmpEntryDir, entryDir); retryErr != nil {
			return nil, err
		}
	}
	return manifest, nil
}

func (s *Store) Restore(worktree string, taskName, key string) (bool, error) {
	manifest, ok, err := s.Load(taskName, key)
	if err != nil || !ok {
		return ok, err
	}
	entryDir := s.EntryDir(taskName, key)

	for _, rel := range manifest.Outputs.Files {
		if err := fsutil.RemoveIfExists(filepath.Join(worktree, rel)); err != nil {
			_ = os.RemoveAll(entryDir)
			return false, nil
		}
	}
	for _, rel := range manifest.Outputs.Dirs {
		if err := fsutil.RemoveIfExists(filepath.Join(worktree, rel)); err != nil {
			_ = os.RemoveAll(entryDir)
			return false, nil
		}
	}

	for i, rel := range manifest.Outputs.Files {
		if err := fsutil.CopyFile(filepath.Join(entryDir, "files", strconv.Itoa(i)), filepath.Join(worktree, rel)); err != nil {
			_ = os.RemoveAll(entryDir)
			return false, nil
		}
	}
	for i, rel := range manifest.Outputs.Dirs {
		if err := fsutil.CopyDir(filepath.Join(entryDir, "dirs", strconv.Itoa(i)), filepath.Join(worktree, rel)); err != nil {
			_ = os.RemoveAll(entryDir)
			return false, nil
		}
	}
	return true, nil
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
		path = filepath.Join(path, task)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return os.RemoveAll(path)
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
			if err := os.RemoveAll(s.EntryDir(filtered[i].Task, filtered[i].Key)); err != nil {
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
