package projectversion

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVerifyAdapterChecksActualSelectedModule(t *testing.T) {
	// The helper module requires a higher Devflow version. Ordinary Go minimal
	// version selection can therefore raise the adapter's direct requirement.
	modules := []struct {
		path, version, requirement string
	}{
		{ModulePath, "v1.2.3", ""},
		{ModulePath, "v1.3.0", ""},
		{"example.test/devflow-fork", "v1.5.0", ""},
		{"example.test/upgrade", "v1.0.0", "require " + ModulePath + " v1.3.0\n"},
	}
	responses := make(map[string][]byte)
	for _, module := range modules {
		mod := []byte("module " + module.path + "\ngo 1.27.1\n" + module.requirement)
		files := map[string][]byte{
			"go.mod":                   mod,
			"pkg/evidence/evidence.go": []byte("package evidence\nconst Version = \"" + module.version + "\"\n"),
		}
		if module.path == "example.test/upgrade" {
			files["upgrade.go"] = []byte("package upgrade\nimport \"github.com/benjaco/devflow/pkg/evidence\"\nfunc Version() string { return evidence.Version }\n")
		}
		var archive bytes.Buffer
		writer := zip.NewWriter(&archive)
		for name, data := range files {
			file, err := writer.Create(module.path + "@" + module.version + "/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		prefix := "/" + module.path + "/@v/" + module.version
		responses[prefix+".mod"] = mod
		responses[prefix+".info"] = []byte(`{"Version":"` + module.version + `","Time":"2026-09-10T00:00:00Z"}`)
		responses[prefix+".zip"] = archive.Bytes()
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if data, ok := responses[r.URL.Path]; ok {
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
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
		if err := filepath.WalkDir(moduleCache, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chmod(path, 0o700)
		}); err != nil {
			t.Error(err)
		}
	})

	for _, scenario := range []string{"canonical", "upgraded by another dependency", "versioned fork", "local source"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			projectModule := "module example.test/app\nrequire github.com/benjaco/devflow v1.2.3\n"
			if scenario == "versioned fork" {
				projectModule += "replace github.com/benjaco/devflow v1.2.3 => example.test/devflow-fork v1.5.0\n"
			}
			if scenario == "local source" {
				source := filepath.Join(root, "source")
				if err := os.MkdirAll(filepath.Join(source, "pkg", "evidence"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte("module github.com/benjaco/devflow\ngo 1.27.1\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, "pkg", "evidence", "evidence.go"), []byte("package evidence\nconst Version = \"local\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				projectModule += "replace github.com/benjaco/devflow => ./source\n"
			}
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(projectModule), 0o600); err != nil {
				t.Fatal(err)
			}
			selected, err := Resolve(root)
			if err != nil {
				t.Fatal(err)
			}
			buildDir := t.TempDir()
			generated, err := selected.ModuleSource("example.test/adapter")
			if err != nil {
				t.Fatal(err)
			}
			main := "package main\nimport (\"fmt\"; \"github.com/benjaco/devflow/pkg/evidence\")\nfunc main() { fmt.Println(evidence.Version) }\n"
			if scenario == "upgraded by another dependency" {
				generated = append(generated, []byte("\nrequire example.test/upgrade v1.0.0\n")...)
				main = "package main\nimport (\"fmt\"; \"example.test/upgrade\")\nfunc main() { fmt.Println(upgrade.Version()) }\n"
			}
			if err := os.WriteFile(filepath.Join(buildDir, "go.mod"), generated, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(buildDir, "main.go"), []byte(main), 0o600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(buildDir, "adapter")
			if runtime.GOOS == "windows" {
				binary += ".exe"
			}
			command := exec.Command("go", "build", "-mod=mod", "-o", binary, ".")
			command.Dir = buildDir
			command.Env = BuildEnv(os.Environ())
			if out, err := command.CombinedOutput(); err != nil {
				t.Fatalf("build adapter: %v\n%s", err, out)
			}
			err = VerifyAdapter(binary, selected)
			if scenario == "upgraded by another dependency" {
				out, runErr := exec.Command(binary).CombinedOutput()
				if runErr != nil || string(out) != "v1.3.0\n" {
					t.Fatalf("fixture did not reproduce Go's dependency upgrade: %q, %v", out, runErr)
				}
				if err == nil || !strings.Contains(err.Error(), "v1.3.0") || !strings.Contains(err.Error(), "v1.2.3") {
					t.Fatalf("adapter silently changed the project pin: %v", err)
				}
			} else if err != nil {
				t.Fatalf("matching adapter was rejected: %v", err)
			}
		})
	}
}
