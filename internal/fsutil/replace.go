package fsutil

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type replacementLock struct {
	mu   sync.Mutex
	refs int
}

var replacementLockRegistry = struct {
	sync.Mutex
	byPath map[string]*replacementLock
}{
	byPath: make(map[string]*replacementLock),
}

// ReplaceFile atomically replaces newPath with oldPath. Replacements targeting
// the same path are serialized within the process; the platform implementation
// handles transient operating-system replacement conflicts.
func ReplaceFile(oldPath, newPath string) error {
	unlock := lockReplacementPath(newPath)
	defer unlock()
	return replaceFile(oldPath, newPath)
}

func lockReplacementPath(path string) func() {
	key, err := filepath.Abs(path)
	if err != nil {
		key = filepath.Clean(path)
	}
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}

	replacementLockRegistry.Lock()
	entry := replacementLockRegistry.byPath[key]
	if entry == nil {
		entry = &replacementLock{}
		replacementLockRegistry.byPath[key] = entry
	}
	entry.refs++
	replacementLockRegistry.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		replacementLockRegistry.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(replacementLockRegistry.byPath, key)
		}
		replacementLockRegistry.Unlock()
	}
}
