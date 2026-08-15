package fsutil

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultCopyMaxFiles int64 = 1_000_000
	DefaultCopyMaxBytes int64 = 20 << 30
)

type CopyEntry struct {
	Path string
	Info fs.FileInfo
}

type CopyProgress struct {
	Path  string `json:"path,omitempty"`
	Files int64  `json:"files"`
	Bytes int64  `json:"bytes"`
	Done  bool   `json:"done,omitempty"`
}

type CopyOptions struct {
	// MaxFiles and MaxBytes default to conservative finite limits when zero.
	// Set either value below zero only for a caller that deliberately wants no
	// limit.
	MaxFiles   int64
	MaxBytes   int64
	Include    func(CopyEntry) bool
	OnProgress func(CopyProgress)
}

type Copier struct {
	options CopyOptions
	files   int64
	bytes   int64
}

func NewCopier(options CopyOptions) *Copier {
	if options.MaxFiles == 0 {
		options.MaxFiles = DefaultCopyMaxFiles
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = DefaultCopyMaxBytes
	}
	return &Copier{options: options}
}

func (c *Copier) Progress() CopyProgress {
	if c == nil {
		return CopyProgress{}
	}
	return CopyProgress{Files: c.files, Bytes: c.bytes}
}

// Copy projects source into destination. projectionRoot is the source-side
// security boundary used for relative paths and symlink validation. A Copier
// can be reused for several Copy calls so its byte/file limits cover the whole
// logical projection.
func (c *Copier) Copy(ctx context.Context, projectionRoot, source, destination string) error {
	if c == nil {
		return fmt.Errorf("filesystem copier is nil")
	}
	root, err := filepath.Abs(projectionRoot)
	if err != nil {
		return fmt.Errorf("resolve copy projection root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve copy projection root %q: %w", projectionRoot, err)
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolve copy source: %w", err)
	}
	sourceParent, err := filepath.EvalSymlinks(filepath.Dir(source))
	if err != nil {
		return fmt.Errorf("resolve copy source parent %q: %w", filepath.Dir(source), err)
	}
	source = filepath.Join(sourceParent, filepath.Base(source))
	inside, err := pathInside(root, source)
	if err != nil {
		return err
	}
	if !inside {
		return fmt.Errorf("copy source %q is outside projection root %q", source, root)
	}
	if err := c.copyPath(ctx, root, source, destination); err != nil {
		return err
	}
	c.report(relativeCopyPath(root, source), true)
	return nil
}

func (c *Copier) copyPath(ctx context.Context, projectionRoot, source, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	rel := relativeCopyPath(projectionRoot, source)
	if c.options.Include != nil && !c.options.Include(CopyEntry{Path: rel, Info: info}) {
		return nil
	}
	if err := c.reserveFile(rel); err != nil {
		return err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return c.copySymlink(projectionRoot, source, destination, rel)
	}
	if info.IsDir() {
		return c.copyDirectory(ctx, projectionRoot, source, destination, rel, info)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cannot copy special file %q (%s)", rel, info.Mode())
	}
	return c.copyRegularFile(source, destination, rel, info)
}

func (c *Copier) copyDirectory(ctx context.Context, projectionRoot, source, destination, rel string, info fs.FileInfo) error {
	restoreParent, err := prepareDestinationParent(destination)
	if err != nil {
		return err
	}
	defer func() { _ = restoreParent() }()
	if existing, err := os.Lstat(destination); err == nil && !existing.IsDir() {
		if err := RemoveAllWritable(destination); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	// Existing restored/cache directories can already be read-only. Keep the
	// destination writable until every child has been copied.
	if err := os.Chmod(destination, info.Mode().Perm()|0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := c.copyPath(ctx, projectionRoot, filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
			return err
		}
	}
	if err := os.Chmod(destination, info.Mode().Perm()); err != nil {
		return err
	}
	_ = os.Chtimes(destination, info.ModTime(), info.ModTime())
	c.report(rel, false)
	return nil
}

func (c *Copier) copySymlink(projectionRoot, source, destination, rel string) error {
	target, err := os.Readlink(source)
	if err != nil {
		return fmt.Errorf("read symlink %q: %w", rel, err)
	}
	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return fmt.Errorf("symlink %q has an absolute target; only internal relative symlinks are allowed", rel)
	}
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("resolve symlink %q: %w", rel, err)
	}
	inside, err := pathInside(projectionRoot, resolved)
	if err != nil {
		return err
	}
	if !inside {
		return fmt.Errorf("symlink %q resolves outside copy projection", rel)
	}
	restoreParent, err := prepareDestinationParent(destination)
	if err != nil {
		return err
	}
	defer func() { _ = restoreParent() }()
	if err := RemoveAllWritable(destination); err != nil {
		return err
	}
	if err := os.Symlink(target, destination); err != nil {
		return fmt.Errorf("preserve symlink %q: %w", rel, err)
	}
	c.report(rel, false)
	return nil
}

func (c *Copier) copyRegularFile(source, destination, rel string, info fs.FileInfo) error {
	if err := c.reserveBytes(rel, info.Size()); err != nil {
		return err
	}
	restoreParent, err := prepareDestinationParent(destination)
	if err != nil {
		return err
	}
	defer func() { _ = restoreParent() }()
	if err := RemoveAllWritable(destination); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = RemoveAllWritable(destination)
		return copyErr
	}
	if closeErr != nil {
		_ = RemoveAllWritable(destination)
		return closeErr
	}
	if written != info.Size() {
		_ = RemoveAllWritable(destination)
		return fmt.Errorf("source file %q changed while copying: expected %d bytes, copied %d", rel, info.Size(), written)
	}
	if err := os.Chmod(destination, info.Mode().Perm()); err != nil {
		return err
	}
	_ = os.Chtimes(destination, info.ModTime(), info.ModTime())
	c.report(rel, false)
	return nil
}

func (c *Copier) reserveFile(rel string) error {
	next := c.files + 1
	if c.options.MaxFiles >= 0 && next > c.options.MaxFiles {
		return fmt.Errorf("copy file-count limit exceeded at %q: limit %d", rel, c.options.MaxFiles)
	}
	c.files = next
	return nil
}

func (c *Copier) reserveBytes(rel string, size int64) error {
	if size < 0 {
		return fmt.Errorf("copy source %q has invalid size %d", rel, size)
	}
	next := c.bytes + size
	if next < c.bytes || c.options.MaxBytes >= 0 && next > c.options.MaxBytes {
		return fmt.Errorf("copy byte limit exceeded at %q: limit %d bytes", rel, c.options.MaxBytes)
	}
	c.bytes = next
	return nil
}

func (c *Copier) report(path string, done bool) {
	if c.options.OnProgress == nil {
		return
	}
	c.options.OnProgress(CopyProgress{Path: filepath.ToSlash(path), Files: c.files, Bytes: c.bytes, Done: done})
}

func prepareDestinationParent(destination string) (func() error, error) {
	parent := filepath.Dir(destination)
	info, err := os.Lstat(parent)
	if os.IsNotExist(err) {
		// These may be structural parents of a standalone cached file rather
		// than source directories whose final mode will be applied later. Keep
		// the historical traversable default while ensuring owner write access.
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return nil, err
		}
		return func() error { return nil }, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("copy destination parent %q is a symlink", parent)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("copy destination parent %q is not a directory", parent)
	}
	originalMode := info.Mode().Perm()
	writableMode := originalMode | 0o700
	if writableMode == originalMode {
		return func() error { return nil }, nil
	}
	if err := os.Chmod(parent, writableMode); err != nil {
		return nil, err
	}
	return func() error { return os.Chmod(parent, originalMode) }, nil
}

func relativeCopyPath(root, source string) string {
	rel, err := filepath.Rel(root, source)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

func pathInside(root, candidate string) (bool, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false, err
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel), nil
}
