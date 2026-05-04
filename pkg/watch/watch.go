package watch

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	filewatcher "github.com/radovskyb/watcher"
)

const (
	DefaultDebounce     = 300 * time.Millisecond
	DefaultPollInterval = 500 * time.Millisecond
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
	ignorePaths   []string
	includePaths  []string
	explicitPaths []string
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
	explicitPaths := append(append([]string{}, includePaths...), watchPaths...)
	return &Runner{
		root:          root,
		debounce:      debounce,
		pollInterval:  pollInterval,
		watchPaths:    watchPaths,
		ignorePaths:   ignorePaths,
		includePaths:  includePaths,
		explicitPaths: explicitPaths,
	}, nil
}

func (r *Runner) Start(ctx context.Context) (<-chan Batch, <-chan error, error) {
	w := filewatcher.New()
	w.FilterOps(filewatcher.Create, filewatcher.Write, filewatcher.Remove, filewatcher.Rename, filewatcher.Move)
	w.AddFilterHook(func(info os.FileInfo, fullPath string) error {
		rel, err := filepath.Rel(r.root, fullPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if pathIncluded(rel, r.explicitPaths) {
			return nil
		}
		for _, ignore := range r.ignorePaths {
			ignore = filepath.ToSlash(ignore)
			if rel == ignore || strings.HasPrefix(rel, ignore+"/") {
				return filewatcher.ErrSkip
			}
		}
		return nil
	})
	if len(r.watchPaths) == 0 {
		if err := w.AddRecursive(r.root); err != nil {
			return nil, nil, err
		}
	} else {
		for _, watchPath := range r.watchPaths {
			if err := addWatchPath(w, r.root, watchPath); err != nil {
				return nil, nil, err
			}
		}
	}
	for _, include := range r.includePaths {
		if err := os.MkdirAll(filepath.Join(r.root, filepath.FromSlash(include)), 0o755); err != nil {
			return nil, nil, err
		}
		if err := addWatchPath(w, r.root, include); err != nil {
			return nil, nil, err
		}
	}

	batches := make(chan Batch, 16)
	errs := make(chan error, 16)

	go func() {
		defer close(batches)
		defer close(errs)
		defer w.Close()

		go func() {
			if err := w.Start(r.pollInterval); err != nil {
				select {
				case errs <- err:
				default:
				}
			}
		}()

		var (
			pending   = map[string]bool{}
			startedAt time.Time
			timer     *time.Timer
			timerCh   <-chan time.Time
		)

		flush := func() {
			if len(pending) == 0 {
				return
			}
			files := make([]string, 0, len(pending))
			for file := range pending {
				files = append(files, file)
			}
			sort.Strings(files)
			batches <- Batch{
				Files:      files,
				StartedAt:  startedAt,
				FinishedAt: time.Now().UTC(),
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
			case err, ok := <-w.Error:
				if !ok {
					return
				}
				if err == nil {
					continue
				}
				select {
				case errs <- err:
				default:
				}
			case evt, ok := <-w.Event:
				if !ok {
					return
				}
				rel, err := filepath.Rel(r.root, evt.Path)
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					continue
				}
				rel = filepath.ToSlash(rel)
				if rel == "." {
					continue
				}
				if len(pending) == 0 {
					startedAt = time.Now().UTC()
				}
				pending[rel] = true
				if timer == nil {
					timer = time.NewTimer(r.debounce)
					timerCh = timer.C
				} else {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(r.debounce)
				}
			case <-timerCh:
				flush()
			}
		}
	}()

	return batches, errs, nil
}

func addWatchPath(w *filewatcher.Watcher, root, rel string) error {
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err == nil {
		if info.IsDir() {
			return w.AddRecursive(full)
		}
		return w.Add(full)
	}
	if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(full)
	for {
		info, statErr := os.Stat(parent)
		if statErr == nil && info.IsDir() {
			return w.AddRecursive(parent)
		}
		next := filepath.Dir(parent)
		relToRoot, relErr := filepath.Rel(root, next)
		if next == parent || relErr != nil || strings.HasPrefix(filepath.ToSlash(relToRoot), "../") || relToRoot == ".." {
			return w.AddRecursive(root)
		}
		parent = next
	}
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
		if path == "." || path == "" || strings.HasPrefix(path, "../") || path == ".." {
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
		if pathIncluded(path, compacted) {
			continue
		}
		compacted = append(compacted, path)
	}
	return compacted
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
