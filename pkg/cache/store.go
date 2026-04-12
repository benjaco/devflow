package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"devflow/internal/fsutil"
	"devflow/internal/jsonutil"
	"devflow/pkg/project"
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
	Root string
}

func New(root string) *Store {
	return &Store{Root: root}
}

func (s *Store) EntryDir(task, key string) string {
	return filepath.Join(s.Root, "entries", task, key)
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
	if err := os.RemoveAll(entryDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(entryDir, "files"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(entryDir, "dirs"), 0o755); err != nil {
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
		if err := fsutil.CopyFile(src, filepath.Join(entryDir, "files", strconv.Itoa(i))); err != nil {
			return nil, err
		}
	}
	for i, rel := range dirs {
		src := filepath.Join(worktree, rel)
		if _, err := os.Stat(src); err != nil {
			return nil, fmt.Errorf("declared output dir %q missing: %w", rel, err)
		}
		if err := fsutil.CopyDir(src, filepath.Join(entryDir, "dirs", strconv.Itoa(i))); err != nil {
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
	if err := jsonutil.WriteFileAtomic(s.manifestPath(task.Name, key), manifest); err != nil {
		return nil, err
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
