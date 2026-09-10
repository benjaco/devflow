package projectversion

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

func TestResolveProjectPin(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	module := []byte("module example.test/app\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v1.2.3\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), module, 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if selected == nil || selected.Version != "v1.2.3" || selected.Worktree != canonicalRoot || selected.Identity == "" {
		t.Fatalf("project pin = %+v; want v1.2.3 with worktree and identity", selected)
	}
	before := selected.Identity
	if err := os.WriteFile(filepath.Join(root, "go.mod"), bytes.ReplaceAll(module, []byte("v1.2.3"), []byte("v1.2.4")), 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err = Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if selected == nil || selected.Version != "v1.2.4" || selected.Identity == before {
		t.Fatalf("updated project pin = %+v; want a new v1.2.4 identity", selected)
	}
}

func TestResolveFilesystemAliasesShareRuntimeIdentity(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte("module github.com/benjaco/devflow\ngo 1.27.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("source", filepath.Join(root, "source-alias")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	alias := filepath.Join(parent, "project-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	module := []byte("module example.test/app\nrequire github.com/benjaco/devflow v1.2.3\nreplace github.com/benjaco/devflow => ./source-alias\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), module, 0o600); err != nil {
		t.Fatal(err)
	}
	direct, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	indirect, err := Resolve(alias)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if direct.Worktree != canonicalRoot || indirect.Worktree != canonicalRoot || direct.SourceRoot != canonicalSource || indirect.SourceRoot != canonicalSource {
		t.Fatalf("worktree/source aliases remained in selections: direct=%+v, indirect=%+v", direct, indirect)
	}
	if direct.Identity != indirect.Identity || BinaryPath(direct) != BinaryPath(indirect) {
		t.Fatalf("same worktree produced separate runtime caches: direct=%s, indirect=%s", BinaryPath(direct), BinaryPath(indirect))
	}
}

func TestResolveNoExplicitPin(t *testing.T) {
	for name, module := range map[string]string{
		"no module file":   "",
		"no requirement":   "module example.test/app\n\ngo 1.27.1\n",
		"replacement only": "module example.test/app\n\ngo 1.27.1\nreplace github.com/benjaco/devflow => example.test/fork v1.0.0\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if module != "" {
				if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(module), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			selected, err := Resolve(root)
			if err != nil || selected != nil {
				t.Fatalf("unpinned project selected %+v, %v", selected, err)
			}
		})
	}
}

func TestResolveVersionReplacementAndModuleIsolation(t *testing.T) {
	root := t.TempDir()
	module := "module example.test/app\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v1.2.3\n" +
		"replace github.com/benjaco/devflow => example.test/default v1.4.0\n" +
		"replace github.com/benjaco/devflow v1.2.3 => example.test/selected v1.5.0\n" +
		"replace github.com/benjaco/devflow v1.2.4 => example.test/next v1.6.0\n" +
		"replace example.test/unrelated => ./does-not-exist\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(module), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.sum"), []byte("checksum snapshot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ReplacementPath != "example.test/selected" || selected.ReplacementVersion != "v1.5.0" || selected.SourceRoot != "" {
		t.Fatalf("replacement = %+v; want exact version replacement", selected)
	}
	generated, err := selected.ModuleSource("devflow.local/runtime")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := modfile.Parse("go.mod", generated, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Require) != 1 || parsed.Require[0].Mod.Path != ModulePath || parsed.Require[0].Mod.Version != "v1.2.3" {
		t.Fatalf("generated requirement = %s", generated)
	}
	if len(parsed.Replace) != 1 || parsed.Replace[0].Old.Version != "v1.2.3" || parsed.Replace[0].New.Path != "example.test/selected" || parsed.Replace[0].New.Version != "v1.5.0" {
		t.Fatalf("generated replacements = %s", generated)
	}
	sum := selected.SumBytes()
	sum[0] = '!'
	if string(selected.SumBytes()) != "checksum snapshot\n" {
		t.Fatal("caller mutated the captured checksum snapshot")
	}
	before := selected.Identity
	if err := os.WriteFile(filepath.Join(root, "go.sum"), []byte("new checksum snapshot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err = Resolve(root)
	if err != nil || selected.Identity == before {
		t.Fatalf("checksum change did not change runtime identity: %+v, %v", selected, err)
	}
}

func TestResolveRejectsInvalidPinsAndReadFailures(t *testing.T) {
	for _, requirement := range []string{"latest", "master", "v2.0.0", "v1.2.3 garbage"} {
		t.Run(requirement, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/app\nrequire github.com/benjaco/devflow "+requirement+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if selected, err := Resolve(root); err == nil || selected != nil {
				t.Fatalf("invalid pin selected %+v, %v", selected, err)
			}
		})
	}
	t.Run("go.mod is a directory", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "go.mod"), 0o755); err != nil {
			t.Fatal(err)
		}
		if selected, err := Resolve(root); err == nil || selected != nil {
			t.Fatalf("go.mod read failure selected %+v, %v", selected, err)
		}
	})
	t.Run("go.sum is a directory", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/app\nrequire github.com/benjaco/devflow v1.2.3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "go.sum"), 0o755); err != nil {
			t.Fatal(err)
		}
		if selected, err := Resolve(root); err == nil || selected != nil || !strings.Contains(err.Error(), "go.sum") {
			t.Fatalf("go.sum read failure selected %+v, %v", selected, err)
		}
	})
}

func TestResolveRejectsExcludedPinAndConflictingReplacements(t *testing.T) {
	for name, declaration := range map[string]string{
		"excluded pin": "exclude github.com/benjaco/devflow v1.2.3\n",
		"conflicting replacement": "replace github.com/benjaco/devflow => example.test/one v1.0.0\n" +
			"replace github.com/benjaco/devflow => example.test/two v1.0.0\n",
		"missing local source": "replace github.com/benjaco/devflow => ./missing\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			module := "module example.test/app\nrequire github.com/benjaco/devflow v1.2.3\n" + declaration
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(module), 0o600); err != nil {
				t.Fatal(err)
			}
			if selected, err := Resolve(root); err == nil || selected != nil {
				t.Fatalf("ambiguous/invalid selection accepted: %+v, %v", selected, err)
			}
		})
	}
}

func TestResolveLocalReplacementIncludesEmbeddedSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source checkout")
	if err := os.MkdirAll(filepath.Join(source, "docs_users"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte("module github.com/benjaco/devflow\n\ngo 1.27.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "docs_users", "setup.md"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	module := []byte("module example.test/app\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v1.2.3\nreplace github.com/benjaco/devflow => \"./source checkout\"\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), module, 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if selected == nil || selected.SourceRoot != canonicalSource || selected.ReplacementPath != canonicalSource {
		t.Fatalf("local replacement = %+v; want source %s", selected, source)
	}
	before := selected.Identity
	if err := os.WriteFile(filepath.Join(source, "docs_users", "setup.md"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err = Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Identity == before {
		t.Fatal("embedded documentation edit reused the old runtime identity")
	}
	before = selected.Identity
	if err := os.WriteFile(filepath.Join(source, "docs_users", "irrelevant_test.go"), []byte("package docs_users"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err = Resolve(root)
	if err != nil || selected.Identity != before {
		t.Fatalf("test-only edit changed runtime identity: %+v, %v", selected, err)
	}
}

func TestPrepareBuildsSelectedCommand(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source checkout")
	if err := os.MkdirAll(filepath.Join(source, "cmd", "devflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte("module github.com/benjaco/devflow\n\ngo 1.27.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "cmd", "devflow", "main.go"), []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"selected command\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	module := []byte("module example.test/app\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v1.2.3\nreplace github.com/benjaco/devflow => \"./source checkout\"\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), module, 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	// Both callers share one immutable runtime. The build lock must prevent a
	// second compilation, rather than letting competing publications race.
	type preparation struct {
		binary   string
		progress bytes.Buffer
		err      error
	}
	var calls [2]preparation
	start := make(chan struct{})
	done := make(chan int, 2)
	for index := range calls {
		go func() {
			<-start
			calls[index].binary, calls[index].err = Prepare(context.Background(), selected, &calls[index].progress)
			done <- index
		}()
	}
	close(start)
	<-done
	<-done
	for index := range calls {
		if calls[index].err != nil {
			t.Fatalf("prepare %d: %v\n%s", index, calls[index].err, calls[index].progress.String())
		}
	}
	binary := calls[0].binary
	if calls[1].binary != binary || strings.Count(calls[0].progress.String()+calls[1].progress.String(), "preparing project version") != 1 {
		t.Fatalf("concurrent prepares did not reuse one build: %#v", calls)
	}
	out, err := exec.Command(binary).CombinedOutput()
	if err != nil || string(out) != "selected command\n" {
		t.Fatalf("selected command = %q, %v", out, err)
	}
	if binary != BinaryPath(selected) {
		t.Fatalf("prepared binary %s does not match selection %s", binary, BinaryPath(selected))
	}
	var progress bytes.Buffer
	again, err := Prepare(context.Background(), selected, &progress)
	if err != nil || again != binary || progress.Len() != 0 {
		t.Fatalf("cached prepare = %s, %v; progress %q", again, err, progress.String())
	}
	after, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !bytes.Equal(after, module) {
		t.Fatalf("project go.mod changed: %q, %v", after, err)
	}
}
