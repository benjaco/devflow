package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/fsutil"
	"golang.org/x/mod/modfile"
)

func TestUpgradeProjectModuleProxy(t *testing.T) {
	// Build the real launcher before isolating module downloads to the fixture.
	launcher := buildBootstrapBinary(t)
	isolateJSONContractState(t)
	t.Setenv(envBootstrapRoot, "")
	t.Setenv(envBootstrapEntry, "")
	t.Setenv(envLocalExec, "")
	t.Setenv(envBootstrapModuleVersion, "")
	t.Setenv(envProjectRuntime, "")
	t.Setenv(envLauncherVersion, "")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GONOSUMDB", "")
	t.Setenv("GONOPROXY", "")
	t.Setenv("GOPRIVATE", "")
	const modulePath = "github.com/benjaco/devflow"
	module := []byte("module " + modulePath + "\n\ngo 1.27.1\n")
	responses := make(map[string][]byte)
	for _, version := range []string{"v1.0.0", "v1.2.3"} {
		prefix := "/" + modulePath + "/@v/" + version
		responses[prefix+".mod"] = module
		responses[prefix+".info"] = []byte(fmt.Sprintf(`{"Version":%q,"Time":"2026-09-10T00:00:00Z"}`, version))
		responses[prefix+".zip"] = upgradeModuleArchive(t, modulePath, version, map[string]string{
			"go.mod":                   string(module),
			"cmd/devflow/main.go":      "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"fixture Devflow\") }\n",
			"pkg/evidence/evidence.go": "package evidence\nconst Value = \"fixture dependency\"\n",
		})
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + modulePath + "/@v/list":
			_, _ = io.WriteString(w, "v1.0.0\nv1.2.3\n")
		case "/" + modulePath + "/@latest":
			_, _ = w.Write(responses["/"+modulePath+"/@v/v1.2.3.info"])
		default:
			data, ok := responses[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
		}
	}))
	defer proxy.Close()
	t.Setenv("GOPROXY", proxy.URL)

	for _, test := range []struct {
		name             string
		projectOption    string
		target           string
		brokenModule     bool
		installFails     bool
		updateProject    bool
		applicationGoEnv bool
	}{
		{name: "updates project to installed version", projectOption: "--project", target: "v1.2.3", updateProject: true},
		{name: "JSON default preserves project without prompting", target: "v1.2.3"},
		{name: "explicit false ignores broken project module", projectOption: "--project=false", target: "v1.2.3", brokenModule: true},
		{name: "failed global install preserves project", projectOption: "--project", target: "v9.9.9", installFails: true},
		{name: "latest pins the installed release", projectOption: "--project", target: "latest", updateProject: true},
		{name: "installs for the host despite application Go settings", projectOption: "--project", target: "v1.2.3", updateProject: true, applicationGoEnv: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			moduleCache := t.TempDir()
			t.Setenv("GOMODCACHE", moduleCache)
			t.Cleanup(func() {
				// Go extracts modules read-only; remove this isolated cache before
				// testing's directory cleanup on both Unix and Windows.
				if err := fsutil.RemoveAllWritable(moduleCache); err != nil {
					t.Error(err)
				}
			})
			binDirectory := t.TempDir()
			t.Setenv("GOBIN", binDirectory)
			worktree := t.TempDir()
			writeTestFile(t, filepath.Join(worktree, "go.mod"), "module example.test/upgrade-project\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v1.0.0\n")
			writeTestFile(t, filepath.Join(worktree, "main.go"), "package main\nimport \"github.com/benjaco/devflow/pkg/evidence\"\nfunc main() { _ = evidence.Value }\n")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			baseline := exec.CommandContext(ctx, "go", "mod", "tidy")
			baseline.Dir = worktree
			if output, err := baseline.CombinedOutput(); err != nil {
				t.Fatalf("prepare original project dependency: %v\n%s", err, output)
			}
			if test.brokenModule {
				writeTestFile(t, filepath.Join(worktree, "go.mod"), "invalid project module\n")
			}
			moduleBefore, err := os.ReadFile(filepath.Join(worktree, "go.mod"))
			if err != nil {
				t.Fatal(err)
			}
			sumBefore, err := os.ReadFile(filepath.Join(worktree, "go.sum"))
			if err != nil {
				t.Fatal(err)
			}
			if test.applicationGoEnv {
				otherOS := "windows"
				if runtime.GOOS == "windows" {
					otherOS = "linux"
				}
				t.Setenv("GOOS", otherOS)
				t.Setenv("GOARCH", "386")
				t.Setenv("GOFLAGS", "--unknown-application-build-option")
				t.Setenv("GOWORK", filepath.Join(worktree, "missing.go.work"))
			}
			args := []string{"upgrade"}
			if test.projectOption != "" {
				args = append(args, test.projectOption)
			}
			args = append(args, "--json", "--worktree", worktree, "--version", test.target)
			cmd := exec.CommandContext(ctx, launcher, args...)
			cmd.Dir = worktree
			cmd.Stdin = strings.NewReader("")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			runErr := cmd.Run()
			if (runErr != nil) != test.installFails {
				t.Fatalf("upgrade outcome: %v\nstdout=%s\nstderr=%s", runErr, &stdout, &stderr)
			}
			var result struct {
				Success          bool   `json:"success"`
				Installed        bool   `json:"installed"`
				InstalledVersion string `json:"installedVersion"`
				Project          *struct {
					Updated         bool   `json:"updated"`
					PreviousVersion string `json:"previousVersion"`
					Version         string `json:"version"`
				} `json:"project"`
				Error *struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("upgrade stdout must contain one JSON result: %v\n%s", err, &stdout)
			}
			if result.Success == test.installFails || result.Installed == test.installFails {
				t.Fatalf("upgrade did not distinguish installation outcome: %+v", result)
			}
			installedPath := filepath.Join(binDirectory, "devflow"+testExeSuffix())
			if test.installFails {
				if result.Error == nil || result.Error.Code == "" {
					t.Fatalf("failed install omitted its structured error: %+v", result)
				}
				if _, err := os.Stat(installedPath); !os.IsNotExist(err) {
					t.Fatalf("failed install created an executable: %v", err)
				}
			} else {
				info, err := buildinfo.ReadFile(installedPath)
				if err != nil {
					t.Fatal(err)
				}
				if info.Main.Path != modulePath || info.Main.Version != "v1.2.3" || info.Main.Replace != nil {
					t.Fatalf("installed executable identifies a different release: %+v", info.Main)
				}
				for _, setting := range info.Settings {
					if setting.Key == "GOOS" && setting.Value != runtime.GOOS || setting.Key == "GOARCH" && setting.Value != runtime.GOARCH {
						t.Fatalf("upgrade installed a binary for another host: %+v", setting)
					}
				}
				if test.updateProject && result.InstalledVersion != info.Main.Version {
					t.Fatalf("project update did not report the actual installed release: module=%+v result=%+v", info.Main, result)
				}
			}
			moduleAfter, err := os.ReadFile(filepath.Join(worktree, "go.mod"))
			if err != nil {
				t.Fatal(err)
			}
			sumAfter, err := os.ReadFile(filepath.Join(worktree, "go.sum"))
			if err != nil {
				t.Fatal(err)
			}
			if test.updateProject {
				parsed, err := modfile.Parse("go.mod", moduleAfter, nil)
				if err != nil {
					t.Fatal(err)
				}
				if len(parsed.Require) != 1 || parsed.Require[0].Mod.Path != modulePath || parsed.Require[0].Mod.Version != "v1.2.3" {
					t.Fatalf("project does not pin installed release: %s", moduleAfter)
				}
				if bytes.Equal(sumBefore, sumAfter) || !strings.Contains(string(sumAfter), modulePath+" v1.2.3 h1:") {
					t.Fatalf("project checksums were not updated: %s", sumAfter)
				}
				if result.Project == nil || !result.Project.Updated || result.Project.PreviousVersion != "v1.0.0" || result.Project.Version != "v1.2.3" {
					t.Fatalf("project update evidence is incomplete: %+v", result.Project)
				}
			} else {
				if !bytes.Equal(moduleBefore, moduleAfter) || !bytes.Equal(sumBefore, sumAfter) {
					t.Fatalf("unrequested/failed update changed project files:\ngo.mod=%s\ngo.sum=%s", moduleAfter, sumAfter)
				}
				if result.Project != nil && result.Project.Updated {
					t.Fatalf("reported an unperformed project update: %+v", result.Project)
				}
			}
		})
	}
}

func upgradeModuleArchive(t *testing.T, modulePath, version string, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, content := range files {
		file, err := writer.Create(modulePath + "@" + version + "/" + name)
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
