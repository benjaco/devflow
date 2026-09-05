package engine

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/benjaco/devflow/pkg/project"
)

type watchOutputEvidence struct {
	scopes []watchOutputScope
}

type watchOutputScope struct {
	path      string
	recursive bool
	valid     bool
	states    map[string]watchOutputState
}

type watchOutputState struct {
	exists  bool
	mode    os.FileMode
	size    int64
	modTime time.Time
}

// Capture outputs at producer completion, before a slower sibling or consumer
// can leave time for an external edit that the next watch drain must retain.
func beginWatchOutputs(ctx context.Context, root string, outputs project.Outputs) func() watchOutputEvidence {
	var scopes []watchOutputScope
	missingParents := map[string]bool{}
	add := func(path string, recursive bool) {
		path = normalizeWatchPath(path)
		scopes = append(scopes, watchOutputScope{path: path, recursive: recursive})
		for parent := normalizeWatchPath(filepath.Dir(path)); parent != "." && parent != ""; parent = normalizeWatchPath(filepath.Dir(parent)) {
			if _, err := os.Lstat(filepath.Join(root, parent)); os.IsNotExist(err) {
				missingParents[parent] = true
			}
		}
	}
	for _, path := range outputs.Files {
		add(path, false)
	}
	for _, path := range outputs.Dirs {
		add(path, true)
	}
	for _, path := range outputs.Paths {
		info, err := os.Lstat(filepath.Join(root, path))
		add(path, err == nil && info.IsDir())
	}
	return func() watchOutputEvidence {
		evidence := watchOutputEvidence{}
		for _, scope := range scopes {
			scope.capture(ctx, root)
			evidence.scopes = append(evidence.scopes, scope)
		}
		for parent := range missingParents {
			state, err := readWatchOutputState(filepath.Join(root, parent))
			if err == nil && state.exists && state.mode.IsDir() {
				evidence.scopes = append(evidence.scopes, watchOutputScope{
					path: parent, valid: true, states: map[string]watchOutputState{parent: state},
				})
			}
		}
		return evidence
	}
}

func (s *watchOutputScope) capture(ctx context.Context, root string) {
	if ctx.Err() != nil {
		return
	}
	full := filepath.Join(root, s.path)
	state, err := readWatchOutputState(full)
	if err != nil {
		return
	}
	s.states = map[string]watchOutputState{s.path: state}
	if state.mode.IsDir() {
		s.recursive = true
		err = filepath.WalkDir(full, func(path string, entry os.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			s.states[normalizeWatchPath(rel)] = watchOutputFileState(info)
			return nil
		})
	}
	// A symlink cannot prove the state of descendants without leaving the
	// declared directory tree, so retain their changes conservatively.
	if s.recursive && state.mode&os.ModeSymlink != 0 {
		return
	}
	s.valid = err == nil && ctx.Err() == nil
}

func readWatchOutputState(path string) (watchOutputState, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return watchOutputState{}, nil
	}
	if err != nil {
		return watchOutputState{}, err
	}
	return watchOutputFileState(info), nil
}

func watchOutputFileState(info os.FileInfo) watchOutputState {
	state := watchOutputState{exists: true, mode: info.Mode()}
	// Match the watcher's metadata model: child changes already identify
	// directory contents, while directory type and mode remain meaningful.
	if !info.IsDir() {
		state.size = info.Size()
		state.modTime = info.ModTime()
	}
	return state
}

func filterProducedWatchOutputs(root string, files []string, evidence []watchOutputEvidence) []string {
	kept := map[string]bool{}
	for _, file := range files {
		path := normalizeWatchPath(file)
		current, err := readWatchOutputState(filepath.Join(root, path))
		suppress := false
		if err == nil {
			for i := len(evidence) - 1; i >= 0; i-- {
				covered := false
				suppress = true
				for _, scope := range evidence[i].scopes {
					if path != scope.path && !(scope.recursive && watchPathHasPrefix(path, scope.path)) {
						continue
					}
					covered = true
					if !scope.matches(path, current) {
						suppress = false
					}
				}
				if covered {
					// Newer incomplete evidence must not fall back to an older match.
					break
				}
				suppress = false
			}
		}
		if !suppress {
			kept[file] = true
		}
	}
	out := make([]string, 0, len(kept))
	for file := range kept {
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}

func (s watchOutputScope) matches(path string, current watchOutputState) bool {
	if !s.valid || s.states[path] != current {
		return false
	}
	// Explicit watch roots observe symlink targets, which Lstat evidence cannot
	// prove unchanged. Never use link metadata to suppress a target edit.
	if current.mode&os.ModeSymlink != 0 {
		return false
	}
	// An absent entry under a complete directory snapshot normally proves a
	// producer deletion, but symlink descendants were deliberately not scanned.
	for parent := normalizeWatchPath(filepath.Dir(path)); parent != "." && parent != "" && watchPathHasPrefix(parent, s.path); parent = normalizeWatchPath(filepath.Dir(parent)) {
		if s.states[parent].mode&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}
