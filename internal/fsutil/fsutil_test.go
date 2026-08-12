package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCopyDirAndRemoveIfExists(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "value.txt"), []byte("stable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CopyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "nested", "value.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "stable" {
		t.Fatalf("copied content = %q", data)
	}
	if err := RemoveIfExists(dst); err != nil {
		t.Fatal(err)
	}
	if err := RemoveIfExists(dst); err != nil {
		t.Fatalf("second remove should be idempotent: %v", err)
	}
}

func TestWriteEnvFileSortedAtomicAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "runtime.env")
	if err := WriteEnvFile(path, map[string]string{"Z": "last", "A": "first"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "A=first\nZ=last\n"; got != want {
		t.Fatalf("env contents = %q, want %q", got, want)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm() & 0o077; got != 0 {
			t.Fatalf("runtime env exposes group/other permissions: %03o", got)
		}
	}
	if err := WriteEnvFile(path, map[string]string{"B": "replacement"}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "B=replacement\n"; got != want {
		t.Fatalf("replacement env contents = %q, want %q", got, want)
	}
}

func TestRealpathResolvesSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	got, err := Realpath(link)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("realpath = %q, want %q", got, want)
	}
}
