package watch

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	DefaultDebounce     = 300 * time.Millisecond
	DefaultPollInterval = 250 * time.Millisecond
)

var defaultIgnorePaths = []string{
	".devflow",
	".git",
	"node_modules",
}

type Options struct {
	Root         string
	Debounce     time.Duration
	PollInterval time.Duration
	WatchPaths   []string
	WatchOnly    bool
	IgnorePaths  []string
	IncludePaths []string
}

type Batch struct {
	Files      []string
	StartedAt  time.Time
	FinishedAt time.Time
}

type Runner struct {
	root          string
	debounce      time.Duration
	pollInterval  time.Duration
	watchPaths    []string
	watchOnly     bool
	ignorePaths   []string
	includePaths  []string
	explicitPaths []string
}

type fileState struct {
	modTime time.Time
	mode    os.FileMode
	size    int64
	isDir   bool
}

func New(opts Options) (*Runner, error) {
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	debounce := opts.Debounce
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	ignorePaths := append([]string{}, defaultIgnorePaths...)
	ignorePaths = append(ignorePaths, opts.IgnorePaths...)
	watchPaths := normalizeIncludePaths(root, opts.WatchPaths)
	includePaths := normalizeIncludePaths(root, opts.IncludePaths)
	explicitPaths := make([]string, 0, len(includePaths)+len(watchPaths))
	for _, paths := range [][]string{includePaths, watchPaths} {
		for _, path := range paths {
			// Root-level inputs and globs require a root scan, but do not
			// opt every ignored subtree into watching.
			if path != "." {
				explicitPaths = append(explicitPaths, path)
			}
		}
	}
	return &Runner{
		root:          root,
		debounce:      debounce,
		pollInterval:  pollInterval,
		watchPaths:    watchPaths,
		watchOnly:     opts.WatchOnly,
		ignorePaths:   ignorePaths,
		includePaths:  includePaths,
		explicitPaths: explicitPaths,
	}, nil
}

func (r *Runner) Start(ctx context.Context) (<-chan Batch, <-chan error, error) {
	for _, include := range r.includePaths {
		if err := os.MkdirAll(filepath.Join(r.root, filepath.FromSlash(include)), 0o755); err != nil {
			return nil, nil, err
		}
	}
	previous, err := r.snapshot()
	if err != nil {
		return nil, nil, err
	}

	batches := make(chan Batch, 16)
	errs := make(chan error, 16)

	go func() {
		defer close(batches)
		defer close(errs)

		var (
			pending   = map[string]bool{}
			startedAt time.Time
			timer     *time.Timer
			timerCh   <-chan time.Time
		)
		ticker := time.NewTicker(r.pollInterval)
		defer ticker.Stop()
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()

		flush := func() {
			if len(pending) == 0 {
				return
			}
			files := make([]string, 0, len(pending))
			for file := range pending {
				files = append(files, file)
			}
			sort.Strings(files)
			batch := Batch{
				Files:      files,
				StartedAt:  startedAt,
				FinishedAt: time.Now().UTC(),
			}
			select {
			case batches <- batch:
			case <-ctx.Done():
				return
			}
			pending = map[string]bool{}
			startedAt = time.Time{}
			if timer != nil {
				timer.Stop()
			}
			timer = nil
			timerCh = nil
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, err := r.snapshot()
				if err == nil {
					changes := changedFiles(previous, current)
					previous = current
					if len(changes) == 0 {
						continue
					}
					if len(pending) == 0 {
						startedAt = time.Now().UTC()
					}
					addedNewFile := false
					for _, rel := range changes {
						if rel != "." && rel != "" {
							if !pending[rel] {
								addedNewFile = true
							}
							pending[rel] = true
						}
					}
					if timer == nil {
						timer = time.NewTimer(r.debounce)
						timerCh = timer.C
					} else if addedNewFile {
						if !timer.Stop() {
							select {
							case <-timer.C:
							default:
							}
						}
						timer.Reset(r.debounce)
					}
				} else {
					select {
					case errs <- err:
					default:
					}
				}
			case <-timerCh:
				flush()
			}
		}
	}()

	return batches, errs, nil
}

func (r *Runner) snapshot() (map[string]fileState, error) {
	roots := r.scanRoots()
	out := make(map[string]fileState)
	for _, rel := range roots {
		if err := r.scanPath(rel, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *Runner) scanRoots() []string {
	roots := make([]string, 0, len(r.watchPaths)+len(r.includePaths)+1)
	if len(r.watchPaths) == 0 && !r.watchOnly {
		roots = append(roots, ".")
	} else {
		roots = append(roots, r.watchPaths...)
	}
	roots = append(roots, r.includePaths...)
	return compactScanRoots(roots)
}

func (r *Runner) scanPath(rel string, out map[string]fileState) error {
	rel = filepath.ToSlash(filepath.Clean(rel))
	full := filepath.Join(r.root, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		r.addState(rel, info, out)
		return nil
	}
	return filepath.WalkDir(full, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		itemRel, err := filepath.Rel(r.root, path)
		if err != nil {
			return err
		}
		itemRel = filepath.ToSlash(filepath.Clean(itemRel))
		if itemRel == "." {
			return nil
		}
		if r.ignored(itemRel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		r.addState(itemRel, info, out)
		return nil
	})
}

func (r *Runner) addState(rel string, info os.FileInfo, out map[string]fileState) {
	if rel == "." || rel == "" {
		return
	}
	out[rel] = fileState{
		modTime: info.ModTime(),
		mode:    info.Mode(),
		size:    info.Size(),
		isDir:   info.IsDir(),
	}
}

func (r *Runner) ignored(rel string) bool {
	if pathIncluded(rel, r.explicitPaths) {
		return false
	}
	for _, ignore := range r.ignorePaths {
		ignore = filepath.ToSlash(filepath.Clean(ignore))
		if rel == ignore || strings.HasPrefix(rel, ignore+"/") {
			return true
		}
	}
	return false
}

func changedFiles(previous, current map[string]fileState) []string {
	changed := map[string]bool{}
	for rel, before := range previous {
		after, ok := current[rel]
		if !ok {
			changed[rel] = true
			continue
		}
		if before.modTime != after.modTime || before.mode != after.mode || before.size != after.size || before.isDir != after.isDir {
			changed[rel] = true
		}
	}
	for rel := range current {
		if _, ok := previous[rel]; !ok {
			changed[rel] = true
		}
	}
	out := make([]string, 0, len(changed))
	for rel := range changed {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

func normalizeIncludePaths(root string, paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if filepath.IsAbs(path) {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				continue
			}
			path = rel
		}
		path = filepath.ToSlash(filepath.Clean(path))
		if path == "" || strings.HasPrefix(path, "../") || path == ".." {
			continue
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	sort.Strings(out)
	compacted := make([]string, 0, len(out))
	for _, path := range out {
		// Preserve explicit subtrees beside a root scan, since the root
		// scan skips ignored directories that these paths can opt into.
		if scanRootCovered(path, compacted) {
			continue
		}
		compacted = append(compacted, path)
	}
	return compacted
}

func compactScanRoots(paths []string) []string {
	normalized := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		path = filepath.ToSlash(filepath.Clean(path))
		if path == "" {
			continue
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		normalized = append(normalized, path)
	}
	sort.Strings(normalized)
	out := make([]string, 0, len(normalized))
	for _, path := range normalized {
		if scanRootCovered(path, out) {
			continue
		}
		out = append(out, path)
	}
	return out
}

func scanRootCovered(path string, roots []string) bool {
	for _, root := range roots {
		if root == "." {
			continue
		}
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

func pathIncluded(rel string, includes []string) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	for _, include := range includes {
		if include == "." {
			return true
		}
		if rel == include || strings.HasPrefix(rel, include+"/") {
			return true
		}
	}
	return false
}
