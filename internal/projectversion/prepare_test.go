package projectversion

import (
	"archive/zip"
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/lock"
)

func TestPrepareRemotePinChecksumAndOfflineReuse(t *testing.T) {
	module := []byte("module github.com/benjaco/devflow\n\ngo 1.27.1\n")
	var archive bytes.Buffer
	zipWriter := zip.NewWriter(&archive)
	for name, data := range map[string][]byte{
		"go.mod":              module,
		"cmd/devflow/main.go": []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"remote v1.2.3\") }\n"),
	} {
		file, err := zipWriter.Create(ModulePath + "@v1.2.3/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + ModulePath + "/@v/v1.2.3.mod":
			_, _ = w.Write(module)
		case "/" + ModulePath + "/@v/v1.2.3.info":
			_, _ = io.WriteString(w, `{"Version":"v1.2.3","Time":"2026-09-10T00:00:00Z"}`)
		case "/" + ModulePath + "/@v/v1.2.3.zip":
			_, _ = w.Write(archive.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer proxy.Close()
	t.Setenv("GOPROXY", proxy.URL)
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GONOSUMDB", "")
	t.Setenv("GONOPROXY", "")
	t.Setenv("GOPRIVATE", "")
	moduleCache := t.TempDir()
	t.Setenv("GOMODCACHE", moduleCache)
	t.Cleanup(func() {
		// Go makes extracted modules read-only; restore write access before
		// testing's temporary-directory cleanup on Unix and Windows.
		if err := filepath.WalkDir(moduleCache, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chmod(path, 0o700)
		}); err != nil {
			t.Error(err)
		}
	})
	// These invocation settings belong to application builds, never the launcher.
	t.Setenv("GOWORK", filepath.Join(t.TempDir(), "missing.go.work"))
	t.Setenv("GOFLAGS", "-modfile=missing.mod")
	t.Setenv("GOOS", "plan9")
	t.Setenv("GOARCH", "386")
	root := t.TempDir()
	projectModule := []byte("module example.test/app\nrequire github.com/benjaco/devflow v1.2.3\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), projectModule, 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer
	binary, err := Prepare(context.Background(), selected, &progress)
	if err != nil {
		t.Fatalf("remote prepare: %v\n%s", err, progress.String())
	}
	out, err := exec.Command(binary).CombinedOutput()
	if err != nil || string(out) != "remote v1.2.3\n" {
		t.Fatalf("remote command = %q, %v", out, err)
	}
	t.Setenv("GOPROXY", "off")
	progress.Reset()
	if cached, err := Prepare(context.Background(), selected, &progress); err != nil || cached != binary || progress.Len() != 0 {
		t.Fatalf("offline cached command = %s, %v; progress %q", cached, err, progress.String())
	}
	if after, err := os.ReadFile(filepath.Join(root, "go.mod")); err != nil || !bytes.Equal(after, projectModule) {
		t.Fatalf("tracked go.mod was changed: %q, %v", after, err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.sum")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepare wrote an application go.sum: %v", err)
	}

	badChecksum := []byte(ModulePath + " v1.2.3 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n")
	if err := os.WriteFile(filepath.Join(root, "go.sum"), badChecksum, 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err = Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if failed, err := Prepare(context.Background(), selected, io.Discard); err == nil || failed != "" || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("mismatched checksum = %s, %v; want verification failure", failed, err)
	}
	if after, err := os.ReadFile(filepath.Join(root, "go.sum")); err != nil || !bytes.Equal(after, badChecksum) {
		t.Fatalf("prepare repaired tracked checksum instead of failing: %q, %v", after, err)
	}
	if _, err := os.Stat(BinaryPath(selected)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checksum failure published a selected binary: %v", err)
	}
	if out, err := exec.Command(binary).CombinedOutput(); err != nil || string(out) != "remote v1.2.3\n" {
		t.Fatalf("previous cached version was damaged: %q, %v", out, err)
	}
}

func TestPrepareRemoteReplacementPreservesCanonicalPinAndForkVersion(t *testing.T) {
	const fork = "example.test/devflow-fork"
	module := []byte("module " + fork + "\n\ngo 1.27.1\n")
	var archive bytes.Buffer
	zipWriter := zip.NewWriter(&archive)
	for name, data := range map[string][]byte{
		"go.mod": module,
		"cmd/devflow/main.go": []byte("package main\nimport (\"fmt\"; \"runtime/debug\")\n" +
			"func main() { info, _ := debug.ReadBuildInfo(); fmt.Println(info.Main.Replace.Path, info.Main.Replace.Version) }\n"),
	} {
		file, err := zipWriter.Create(fork + "@v1.5.0/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + fork + "/@v/v1.5.0.mod":
			_, _ = w.Write(module)
		case "/" + fork + "/@v/v1.5.0.info":
			_, _ = io.WriteString(w, `{"Version":"v1.5.0","Time":"2026-09-10T00:00:00Z"}`)
		case "/" + fork + "/@v/v1.5.0.zip":
			_, _ = w.Write(archive.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer proxy.Close()
	t.Setenv("GOPROXY", proxy.URL)
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GONOSUMDB", "")
	t.Setenv("GONOPROXY", "")
	t.Setenv("GOPRIVATE", "")
	moduleCache := t.TempDir()
	t.Setenv("GOMODCACHE", moduleCache)
	t.Cleanup(func() {
		// Go's extracted module directories are read-only, including in this fixture.
		if err := filepath.WalkDir(moduleCache, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chmod(path, 0o700)
		}); err != nil {
			t.Error(err)
		}
	})
	root := t.TempDir()
	projectModule := []byte("module example.test/app\nrequire github.com/benjaco/devflow v1.2.3\n" +
		"replace github.com/benjaco/devflow v1.2.3 => " + fork + " v1.5.0\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), projectModule, 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer
	binary, err := Prepare(context.Background(), selected, &progress)
	if err != nil {
		t.Fatalf("fork prepare: %v\n%s", err, progress.String())
	}
	out, err := exec.Command(binary).CombinedOutput()
	if err != nil || string(out) != fork+" v1.5.0\n" {
		t.Fatalf("selected fork command = %q, %v", out, err)
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if info.Main.Path != ModulePath || info.Main.Version != "v1.2.3" || info.Main.Replace == nil || info.Main.Replace.Path != fork || info.Main.Replace.Version != "v1.5.0" {
		t.Fatalf("selected runtime lost canonical requirement or fork identity: %+v", info.Main)
	}
	t.Setenv("GOPROXY", "off")
	if cached, err := Prepare(context.Background(), selected, io.Discard); err != nil || cached != binary {
		t.Fatalf("cached fork verification = %q, %v", cached, err)
	}
}

func TestPrepareLockCancellationAndChangedSelection(t *testing.T) {
	root := t.TempDir()
	module := []byte("module example.test/app\nrequire github.com/benjaco/devflow v1.2.3\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), module, 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := lock.Acquire(filepath.Join(root, ".devflow", "versions", "prepare.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Prepare(ctx, selected, io.Discard)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("locked prepare cancellation = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prepare did not cancel while the build lock was held")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), bytes.ReplaceAll(module, []byte("v1.2.3"), []byte("v1.2.4")), 0o600); err != nil {
		t.Fatal(err)
	}
	if binary, err := Prepare(context.Background(), selected, io.Discard); err == nil || binary != "" || !strings.Contains(err.Error(), "selection changed") {
		t.Fatalf("obsolete selection = %s, %v; want failure before building", binary, err)
	}
	if _, err := os.Stat(BinaryPath(selected)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled/stale prepare published a binary: %v", err)
	}
}

func TestPrepareRejectsEmptyCachedBinary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/app\nrequire github.com/benjaco/devflow v1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(BinaryPath(selected)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(BinaryPath(selected), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if binary, err := Prepare(context.Background(), selected, io.Discard); err == nil || binary != "" {
		t.Fatalf("empty cached executable accepted: %s, %v", binary, err)
	}
}

func TestBuildEnvPreservesProxyPolicyAndTargetsHost(t *testing.T) {
	env := BuildEnv([]string{"GOPROXY=https://proxy.example", "GOSUMDB=private.example", "GONOPROXY=private/*", "GOWORK=../go.work", "GOFLAGS=-modfile=other.mod", "GOOS=plan9", "GOARCH=386"})
	got := make(map[string]string)
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if _, duplicate := got[key]; duplicate {
			t.Fatalf("duplicate env key %s in %q", key, env)
		}
		got[key] = value
	}
	for key, value := range map[string]string{"GOPROXY": "https://proxy.example", "GOSUMDB": "private.example", "GONOPROXY": "private/*", "GOWORK": "off", "GOFLAGS": "", "GOOS": runtime.GOOS, "GOARCH": runtime.GOARCH} {
		if got[key] != value {
			t.Errorf("%s = %q; want %q", key, got[key], value)
		}
	}
}

func TestTailDiagnosticBounds(t *testing.T) {
	var diagnostic tailBuffer
	if _, err := fmt.Fprint(&diagnostic, strings.Repeat("x", diagnosticLimit*10), "final failure\n"); err != nil {
		t.Fatal(err)
	}
	if len(diagnostic.data) != diagnosticLimit || !strings.HasSuffix(diagnostic.suffix(), "final failure") {
		t.Fatalf("diagnostic length = %d; lost final failure = %v", len(diagnostic.data), !strings.HasSuffix(diagnostic.suffix(), "final failure"))
	}
}
