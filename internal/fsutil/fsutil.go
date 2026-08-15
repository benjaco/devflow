package fsutil

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
)

func Realpath(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

func RemoveIfExists(path string) error {
	return RemoveAllWritable(path)
}

func CopyFile(src, dst string) error {
	copier := NewCopier(CopyOptions{})
	return copier.Copy(context.Background(), filepath.Dir(src), src, dst)
}

func CopyDir(src, dst string) error {
	copier := NewCopier(CopyOptions{})
	return copier.Copy(context.Background(), src, src, dst)
}

func WriteEnvFile(path string, env map[string]string) error {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var data bytes.Buffer
	for _, key := range keys {
		_, _ = data.WriteString(key + "=" + env[key] + "\n")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data.Bytes()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return ReplaceFile(tmpPath, path)
}
