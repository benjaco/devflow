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
	DefaultPollInterval = 100 * time.Millisecond
)

type Options struct {
	Root         string
	Debounce     time.Duration
	PollInterval time.Duration
	IgnorePaths  []string
}

type Batch struct {
	Files      []string
	StartedAt  time.Time
	FinishedAt time.Time
}

type Runner struct {
	root         string
	debounce     time.Duration
	pollInterval time.Duration
	ignorePaths  []string
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
	ignorePaths := append([]string{".devflow", ".git"}, opts.IgnorePaths...)
	return &Runner{
		root:         root,
		debounce:     debounce,
		pollInterval: pollInterval,
		ignorePaths:  ignorePaths,
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
		for _, ignore := range r.ignorePaths {
			ignore = filepath.ToSlash(ignore)
			if rel == ignore || strings.HasPrefix(rel, ignore+"/") {
				return filewatcher.ErrSkip
			}
		}
		return nil
	})
	if err := w.AddRecursive(r.root); err != nil {
		return nil, nil, err
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
			case err := <-w.Error:
				if err == nil {
					continue
				}
				select {
				case errs <- err:
				default:
				}
			case evt := <-w.Event:
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
