//go:build windows

package lock

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type FileLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func Acquire(path string) (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	lock := &FileLock{file: file}
	if err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1,
		0,
		&lock.overlapped,
	); err != nil {
		file.Close()
		return nil, err
	}
	return lock, nil
}

func (l *FileLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	defer func() {
		l.file = nil
	}()
	if err := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped); err != nil {
		_ = l.file.Close()
		return err
	}
	return l.file.Close()
}
