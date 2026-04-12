package lock

import (
	"os"
	"path/filepath"
	"syscall"
)

type FileLock struct {
	file *os.File
}

func Acquire(path string) (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
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
	if err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN); err != nil {
		_ = l.file.Close()
		return err
	}
	return l.file.Close()
}
