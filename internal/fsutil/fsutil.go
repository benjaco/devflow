package fsutil

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

func Realpath(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

func RemoveIfExists(path string) error {
	if _, err := os.Stat(path); err == nil {
		return os.RemoveAll(path)
	} else if os.IsNotExist(err) {
		return nil
	} else {
		return err
	}
}

func CopyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func CopyDir(src, dst string) error {
	paths := make([]string, 0)
	if err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return err
	}
	sort.Strings(paths)

	for _, path := range paths {
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := os.MkdirAll(target, info.Mode()); err != nil {
				return err
			}
			continue
		}
		if err := CopyFile(path, target); err != nil {
			return err
		}
	}
	return nil
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
