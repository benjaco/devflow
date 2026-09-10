package projectversion

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/fsutil"
	"golang.org/x/mod/modfile"
)

func TestUpdateProjectPinAndChecksumsPreservesApplicationModule(t *testing.T) {
	const pseudo = "v1.1.1-0.20260910123456-abcdefabcdef"
	responses := make(map[string][]byte)
	for _, version := range []string{"v1.0.0", "v1.1.0", pseudo} {
		mod := "module " + ModulePath + "\ngo 1.27.1\n"
		prefix := "/" + ModulePath + "/@v/" + version
		responses[prefix+".mod"] = []byte(mod)
		responses[prefix+".info"] = []byte(`{"Version":"` + version + `","Time":"2026-09-10T12:34:56Z"}`)
		responses[prefix+".zip"] = moduleZip(t, ModulePath, version, map[string]string{
			"go.mod": mod, "version.go": "package devflow\nconst Version = \"" + version + "\"\n",
		})
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if data, found := responses[r.URL.Path]; found {
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	defer proxy.Close()
	for _, target := range []string{"v1.1.0", pseudo} {
		t.Run(target, func(t *testing.T) {
			t.Setenv("GOPROXY", proxy.URL)
			t.Setenv("GOSUMDB", "off")
			t.Setenv("GONOSUMDB", "")
			t.Setenv("GONOPROXY", "")
			t.Setenv("GOPRIVATE", "")
			moduleCache := t.TempDir()
			t.Setenv("GOMODCACHE", moduleCache)
			t.Cleanup(func() {
				if err := fsutil.RemoveAllWritable(moduleCache); err != nil {
					t.Error(err)
				}
			})
			parent := t.TempDir()
			root := filepath.Join(parent, "project")
			library := filepath.Join(parent, "local library")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(library, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(library, "go.mod"), []byte("module example.test/local\ngo 1.27.1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(library, "local.go"), []byte("package local\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			module := []byte("module example.test/application\n\ngo 1.27.1\n\nrequire (\n github.com/benjaco/devflow v1.0.0\n example.test/local v0.0.0\n)\n\nreplace example.test/local => \"../local library\"\n")
			if err := os.WriteFile(filepath.Join(root, "go.mod"), module, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package application\nimport (_ \"github.com/benjaco/devflow\"; _ \"example.test/local\")\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			download := exec.Command("go", "mod", "download", ModulePath+"@v1.0.0")
			download.Dir = root
			download.Env = BuildEnv(os.Environ())
			if out, err := download.CombinedOutput(); err != nil {
				t.Fatalf("seed old checksums: %v\n%s", err, out)
			}
			oldSum, err := os.ReadFile(filepath.Join(root, "go.sum"))
			if err != nil {
				t.Fatal(err)
			}
			selected, err := Resolve(root)
			if err != nil {
				t.Fatal(err)
			}
			// Pass a usable invocation proxy explicitly while ambient Go routing
			// would fail; Update must preserve its supplied environment snapshot.
			env := os.Environ()
			t.Setenv("GOPROXY", "off")
			env = append(env, "GOWORK="+filepath.Join(parent, "missing.go.work"), "GOFLAGS=-modfile=missing.mod", "GOOS=plan9", "GOARCH=386")
			var progress bytes.Buffer
			if err := Update(context.Background(), selected, target, env, &progress); err != nil {
				t.Fatalf("update to %s: %v\n%s", target, err, progress.String())
			}
			updated, err := Resolve(root)
			if err != nil || updated.Version != target || updated.ReplacementPath != "" {
				t.Fatalf("updated selection = %+v, %v", updated, err)
			}
			data, err := os.ReadFile(filepath.Join(root, "go.mod"))
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := modfile.Parse("go.mod", data, nil)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Module.Mod.Path != "example.test/application" || len(parsed.Replace) != 1 || parsed.Replace[0].Old.Path != "example.test/local" || parsed.Replace[0].New.Path != "../local library" {
				t.Fatalf("update changed application module or relative replacement: %s", data)
			}
			applicationDependency := false
			for _, require := range parsed.Require {
				if require.Mod.Path == "example.test/local" && require.Mod.Version == "v0.0.0" {
					applicationDependency = true
				}
			}
			if !applicationDependency {
				t.Fatalf("application dependency disappeared: %s", data)
			}
			sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
			if err != nil || bytes.Equal(sum, oldSum) || !bytes.Contains(sum, []byte(ModulePath+" "+target+" h1:")) {
				t.Fatalf("updated checksum evidence absent: %q, %v", sum, err)
			}
		})
	}
}

func TestUpdateRejectsReplacementsAndInvalidVersions(t *testing.T) {
	for name, replacement := range map[string]string{
		"active fork":                     "replace github.com/benjaco/devflow => example.test/fork v1.0.0\n",
		"active local source":             "replace github.com/benjaco/devflow => ./source\n",
		"replacement activates at target": "replace github.com/benjaco/devflow v1.1.0 => example.test/fork v1.0.0\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			module := []byte("module example.test/app\nrequire github.com/benjaco/devflow v1.0.0\n" + replacement)
			if name == "active local source" {
				source := filepath.Join(root, "source")
				if err := os.Mkdir(source, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte("module github.com/benjaco/devflow\ngo 1.27.1\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "go.mod"), module, 0o600); err != nil {
				t.Fatal(err)
			}
			selected, err := Resolve(root)
			if err != nil {
				t.Fatal(err)
			}
			err = Update(context.Background(), selected, "v1.1.0", nil, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "replacement") {
				t.Fatalf("replacement update accepted: %v", err)
			}
			if after, err := os.ReadFile(filepath.Join(root, "go.mod")); err != nil || !bytes.Equal(after, module) {
				t.Fatalf("rejected replacement changed module: %q, %v", after, err)
			}
		})
	}
	for _, target := range []string{"latest", "main", "v2.0.0"} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			module := []byte("module example.test/app\nrequire github.com/benjaco/devflow v1.0.0\n")
			if err := os.WriteFile(filepath.Join(root, "go.mod"), module, 0o600); err != nil {
				t.Fatal(err)
			}
			selected, err := Resolve(root)
			if err != nil {
				t.Fatal(err)
			}
			if err := Update(context.Background(), selected, target, nil, io.Discard); err == nil {
				t.Fatal("accepted a non-exact or incompatible version")
			}
			if after, err := os.ReadFile(filepath.Join(root, "go.mod")); err != nil || !bytes.Equal(after, module) {
				t.Fatalf("rejected version changed module: %q, %v", after, err)
			}
		})
	}
}

func TestUpdateDownloadFailuresPreserveOriginalFiles(t *testing.T) {
	for _, failure := range []string{"unavailable version", "checksum mismatch"} {
		t.Run(failure, func(t *testing.T) {
			mod := "module " + ModulePath + "\ngo 1.27.1\n"
			archive := moduleZip(t, ModulePath, "v1.1.0", map[string]string{"go.mod": mod, "version.go": "package devflow\n"})
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "v1.0.0.mod") || (failure != "unavailable version" && strings.HasSuffix(r.URL.Path, "v1.1.0.mod")) {
					_, _ = io.WriteString(w, mod)
				} else if failure != "unavailable version" && strings.HasSuffix(r.URL.Path, "v1.1.0.info") {
					_, _ = io.WriteString(w, `{"Version":"v1.1.0","Time":"2026-09-10T12:34:56Z"}`)
				} else if failure != "unavailable version" && strings.HasSuffix(r.URL.Path, "v1.1.0.zip") {
					_, _ = w.Write(archive)
				} else {
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
				if err := fsutil.RemoveAllWritable(moduleCache); err != nil {
					t.Error(err)
				}
			})
			root := t.TempDir()
			module := []byte("module example.test/app\ngo 1.27.1\nrequire github.com/benjaco/devflow v1.0.0\n")
			sum := []byte{}
			if failure == "checksum mismatch" {
				sum = []byte(ModulePath + " v1.1.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n")
			}
			if err := os.WriteFile(filepath.Join(root, "go.mod"), module, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "go.sum"), sum, 0o600); err != nil {
				t.Fatal(err)
			}
			selected, err := Resolve(root)
			if err != nil {
				t.Fatal(err)
			}
			err = Update(context.Background(), selected, "v1.1.0", nil, io.Discard)
			if err == nil || (failure == "checksum mismatch" && !strings.Contains(err.Error(), "checksum mismatch")) {
				t.Fatalf("download failure was hidden: %v", err)
			}
			if after, err := os.ReadFile(filepath.Join(root, "go.mod")); err != nil || !bytes.Equal(after, module) {
				t.Fatalf("failed update changed module: %q, %v", after, err)
			}
			if after, err := os.ReadFile(filepath.Join(root, "go.sum")); err != nil || !bytes.Equal(after, sum) {
				t.Fatalf("failed update changed checksums: %q, %v", after, err)
			}
		})
	}
}

func TestUpdateCancellationAndConcurrentEditPreserveOriginalFiles(t *testing.T) {
	for _, operation := range []string{"cancel", "edit project"} {
		t.Run(operation, func(t *testing.T) {
			mod := "module " + ModulePath + "\ngo 1.27.1\n"
			archive := moduleZip(t, ModulePath, "v1.1.0", map[string]string{"go.mod": mod, "version.go": "package devflow\n"})
			entered, release := make(chan struct{}), make(chan struct{})
			var enteredOnce sync.Once
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "v1.1.0.mod") {
					enteredOnce.Do(func() { close(entered) })
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					_, _ = io.WriteString(w, mod)
				} else if strings.HasSuffix(r.URL.Path, "v1.0.0.mod") {
					_, _ = io.WriteString(w, mod)
				} else if strings.HasSuffix(r.URL.Path, "v1.1.0.info") {
					_, _ = io.WriteString(w, `{"Version":"v1.1.0","Time":"2026-09-10T12:34:56Z"}`)
				} else if strings.HasSuffix(r.URL.Path, "v1.1.0.zip") {
					_, _ = w.Write(archive)
				} else {
					http.NotFound(w, r)
				}
			}))
			defer proxy.Close()
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			t.Setenv("GOPROXY", proxy.URL)
			t.Setenv("GOSUMDB", "off")
			t.Setenv("GONOSUMDB", "")
			t.Setenv("GONOPROXY", "")
			t.Setenv("GOPRIVATE", "")
			moduleCache := t.TempDir()
			t.Setenv("GOMODCACHE", moduleCache)
			t.Cleanup(func() {
				if err := fsutil.RemoveAllWritable(moduleCache); err != nil {
					t.Error(err)
				}
			})
			root := t.TempDir()
			module := []byte("module example.test/app\ngo 1.27.1\nrequire github.com/benjaco/devflow v1.0.0\n")
			sum := []byte{}
			if err := os.WriteFile(filepath.Join(root, "go.mod"), module, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "go.sum"), sum, 0o600); err != nil {
				t.Fatal(err)
			}
			selected, err := Resolve(root)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			var updateErr error
			go func() {
				defer close(done)
				updateErr = Update(ctx, selected, "v1.1.0", nil, io.Discard)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("update did not exit after cancellation")
				}
			})
			select {
			case <-entered:
			case <-done:
				t.Fatalf("update exited before download handshake: %v", updateErr)
			case <-time.After(10 * time.Second):
				t.Fatal("update did not reach download handshake")
			}
			if operation == "cancel" {
				cancel()
			} else {
				module = append(module, []byte("// concurrent project edit\n")...)
				sum = []byte("\n")
				if err := os.WriteFile(filepath.Join(root, "go.mod"), module, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "go.sum"), sum, 0o600); err != nil {
					t.Fatal(err)
				}
				close(release)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("update did not finish after cancellation or download release")
			}
			if updateErr == nil || (operation == "cancel" && !errors.Is(updateErr, context.Canceled)) || (operation == "edit project" && !strings.Contains(updateErr.Error(), "selection changed")) {
				t.Fatalf("update lost cancellation/concurrent-edit error: %v", updateErr)
			}
			if after, err := os.ReadFile(filepath.Join(root, "go.mod")); err != nil || !bytes.Equal(after, module) {
				t.Fatalf("update overwrote project module: %q, %v", after, err)
			}
			if after, err := os.ReadFile(filepath.Join(root, "go.sum")); err != nil || !bytes.Equal(after, sum) {
				t.Fatalf("update overwrote project checksums: %q, %v", after, err)
			}
		})
	}
}

// moduleZip only encodes the supplied fixture files; each test keeps its module,
// proxy policy, filesystem setup, execution and assertions visible in place.
func moduleZip(t *testing.T, path, version string, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, content := range files {
		file, err := writer.Create(path + "@" + version + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
