//go:build !windows

package lock

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type FileLock struct {
	file *os.File
}

func Acquire(path string) (*FileLock, error) {
	return acquire(path, false)
}

// TryAcquire acquires an exclusive lock without waiting for another holder.
func TryAcquire(path string) (*FileLock, error) {
	return acquire(path, true)
}

func acquire(path string, nonblocking bool) (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	flags := unix.LOCK_EX
	if nonblocking {
		flags |= unix.LOCK_NB
	}
	if err := unix.Flock(int(file.Fd()), flags); err != nil {
		file.Close()
		if nonblocking && (errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &FileLock{file: file}, nil
}

func (l *FileLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	defer func() {
		l.file = nil
	}()
	if err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN); err != nil {
		_ = l.file.Close()
		return err
	}
	return l.file.Close()
}
