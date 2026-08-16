package fsutil

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestCopyDirPopulatesReadOnlyModuleCacheBeforeRestoringModes(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "gomodcache", "example.com", "readonly@v1.0.0")
	t.Cleanup(func() { _ = RemoveAllWritable(filepath.Join(root, "gomodcache")) })
	if err := os.MkdirAll(filepath.Join(src, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module example.com/readonly\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "internal", "value.go"), []byte("package internal\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "internal"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o555); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(root, "projection")
	if err := CopyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "internal", "value.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "package internal\n" {
		t.Fatalf("copied module content = %q", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dst, "internal"))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o555 {
			t.Fatalf("copied directory mode = %03o, want 555", got)
		}
	}
	if err := RemoveAllWritable(dst); err != nil {
		t.Fatalf("remove read-only copied tree: %v", err)
	}
}

func TestCopyFileTemporarilyMakesDestinationParentWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory permission semantics")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	if err := os.WriteFile(source, []byte("value"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "readonly-parent")
	if err := os.Mkdir(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveAllWritable(parent) })
	if err := CopyFile(source, filepath.Join(parent, "copied.txt")); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(parent); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o555 {
		t.Fatalf("destination parent mode = %03o, want restored 555", got)
	}
}

func TestMovePathWritableMovesReadOnlyDirectoryAndRestoresMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory permission semantics")
	}
	root := t.TempDir()
	t.Cleanup(func() { _ = RemoveAllWritable(root) })
	source := filepath.Join(root, "source", "readonly")
	destination := filepath.Join(root, "holding", "readonly")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("content"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := MovePathWritable(source, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source survived move: %v", err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o555 {
		t.Fatalf("destination mode = %03o, want 555", info.Mode().Perm())
	}
	if data, err := os.ReadFile(filepath.Join(destination, "file.txt")); err != nil || string(data) != "content" {
		t.Fatalf("moved read-only content = %q, err=%v", data, err)
	}
}

func TestCopierPreservesInternalPNPMSymlinksWithoutExpandingGraph(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires developer-mode symlink support")
	}
	root := t.TempDir()
	src := filepath.Join(root, "node_modules")
	pkg := filepath.Join(src, ".pnpm", "pkg@1.0.0", "node_modules", "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "index.js"), []byte("module.exports = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(".pnpm", "pkg@1.0.0", "node_modules", "pkg"), filepath.Join(src, "pkg")); err != nil {
		t.Fatal(err)
	}

	var last CopyProgress
	copier := NewCopier(CopyOptions{OnProgress: func(progress CopyProgress) { last = progress }})
	dst := filepath.Join(root, "copied-node_modules")
	if err := copier.Copy(context.Background(), src, src, dst); err != nil {
		t.Fatal(err)
	}
	linkInfo, err := os.Lstat(filepath.Join(dst, "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("pnpm package link was dereferenced: mode=%s", linkInfo.Mode())
	}
	target, err := os.Readlink(filepath.Join(dst, "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(".pnpm", "pkg@1.0.0", "node_modules", "pkg") {
		t.Fatalf("copied symlink target = %q", target)
	}
	if last.Files > 8 || last.Bytes != int64(len("module.exports = 1\n")) {
		t.Fatalf("copy progress suggests symlink expansion: %+v", last)
	}
}

func TestCopierRejectsExternalRelativeSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires developer-mode symlink support")
	}
	root := t.TempDir()
	src := filepath.Join(root, "projection")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "external.txt")
	if err := os.WriteFile(external, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "external.txt"), filepath.Join(src, "external")); err != nil {
		t.Fatal(err)
	}
	err := NewCopier(CopyOptions{}).Copy(context.Background(), src, src, filepath.Join(root, "dst"))
	if err == nil || !strings.Contains(err.Error(), "outside copy projection") {
		t.Fatalf("expected external symlink rejection, got %v", err)
	}
}

func TestCopierEnforcesLimitsAndReportsProgress(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte("1234"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	progressCalls := 0
	err := NewCopier(CopyOptions{
		MaxFiles: 2,
		MaxBytes: 16,
		OnProgress: func(CopyProgress) {
			progressCalls++
		},
	}).Copy(context.Background(), src, src, filepath.Join(root, "dst-files"))
	if err == nil || !strings.Contains(err.Error(), "file-count limit") {
		t.Fatalf("expected file-count limit, got %v", err)
	}
	if progressCalls == 0 {
		t.Fatal("expected copy progress before the limit failure")
	}

	err = NewCopier(CopyOptions{MaxFiles: 10, MaxBytes: 3}).Copy(context.Background(), src, src, filepath.Join(root, "dst-bytes"))
	if err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("expected byte limit, got %v", err)
	}
}

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

func TestWriteEnvFileConcurrentWritersLeaveCompleteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "runtime.env")
	const writers = 32
	errCh := make(chan error, writers)
	var wg sync.WaitGroup
	for value := 0; value < writers; value++ {
		wg.Add(1)
		go func(value int) {
			defer wg.Done()
			errCh <- WriteEnvFile(path, map[string]string{"VALUE": strconv.Itoa(value)})
		}(value)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent atomic env write failed: %v", err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value := strings.TrimPrefix(strings.TrimSpace(string(data)), "VALUE=")
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 || parsed >= writers {
		t.Fatalf("final env file is incomplete or unexpected: %q", data)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".runtime.env.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary env files were not cleaned up: %v", matches)
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
