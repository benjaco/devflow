package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLocalProjectSourceFilesDiscoversOnlyAdapterSources(t *testing.T) {
	worktree := t.TempDir()
	projectPath := filepath.Join(worktree, localProjectFile)
	writeTestFile(t, projectPath, "package main\n")
	writeTestFile(t, filepath.Join(worktree, "devflow_z.go"), "package main\n")
	writeTestFile(t, filepath.Join(worktree, "devflow_a.go"), "package main\n")
	writeTestFile(t, filepath.Join(worktree, "devflow_watch_test.go"), "not valid Go on purpose\n")
	writeTestFile(t, filepath.Join(worktree, "devflow.project_test.go"), "not valid Go on purpose\n")
	writeTestFile(t, filepath.Join(worktree, "backend.go"), "not valid Go on purpose\n")
	for _, name := range []string{"nested", "devflow_nested"} {
		if err := os.Mkdir(filepath.Join(worktree, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(worktree, "nested", "devflow.project.go"), "not valid Go on purpose\n")
	writeTestFile(t, filepath.Join(worktree, "devflow_nested", "tasks.go"), "not valid Go on purpose\n")

	got, err := localProjectSourceFiles(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		projectPath,
		filepath.Join(worktree, "devflow_a.go"),
		filepath.Join(worktree, "devflow_z.go"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected local project sources:\n got: %v\nwant: %v", got, want)
	}
}

func TestLocalProjectSourceFilesRequiresEntrypoint(t *testing.T) {
	worktree := t.TempDir()
	writeTestFile(t, filepath.Join(worktree, "devflow_shared.go"), "package main\n")

	_, err := localProjectSourceFiles(filepath.Join(worktree, localProjectFile))
	if err == nil || !strings.Contains(err.Error(), "devflow.project.go not found") {
		t.Fatalf("expected compatible missing-entrypoint error, got %v", err)
	}
}

func TestLocalProjectDetectionRequiresEntrypointMarker(t *testing.T) {
	t.Setenv(envBootstrapEntry, "")
	t.Setenv(envLocalExec, "")
	worktree := t.TempDir()
	t.Chdir(worktree)
	writeTestFile(t, filepath.Join(worktree, "devflow_shared.go"), "package main\n")
	if shouldExecLocalProject([]string{"graph", "list"}, worktree) {
		t.Fatal("companion file alone activated local project bootstrap")
	}
	if err := os.Mkdir(filepath.Join(worktree, localProjectFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if !shouldExecLocalProject([]string{"graph", "list"}, worktree) {
		t.Fatal("non-regular entrypoint marker bypassed source validation")
	}
}

func TestLocalProjectSourceFilesRejectsMatchingDirectoryAndSymlink(t *testing.T) {
	t.Run("companion directory", func(t *testing.T) {
		worktree := t.TempDir()
		projectPath := filepath.Join(worktree, localProjectFile)
		writeTestFile(t, projectPath, "package main\n")
		matchingPath := filepath.Join(worktree, "devflow_backend.go")
		if err := os.Mkdir(matchingPath, 0o755); err != nil {
			t.Fatal(err)
		}

		_, err := localProjectSourceFiles(projectPath)
		if err == nil || !strings.Contains(err.Error(), matchingPath) || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("expected path-specific directory error, got %v", err)
		}
	})

	t.Run("companion symlink", func(t *testing.T) {
		worktree := t.TempDir()
		projectPath := filepath.Join(worktree, localProjectFile)
		writeTestFile(t, projectPath, "package main\n")
		writeTestFile(t, filepath.Join(worktree, "backend.go"), "package main\n")
		matchingPath := filepath.Join(worktree, "devflow_backend.go")
		if err := os.Symlink("backend.go", matchingPath); err != nil {
			t.Skipf("symlink creation is unavailable: %v", err)
		}

		_, err := localProjectSourceFiles(projectPath)
		if err == nil || !strings.Contains(err.Error(), matchingPath) || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("expected path-specific symlink error, got %v", err)
		}
	})

	t.Run("entrypoint directory", func(t *testing.T) {
		worktree := t.TempDir()
		projectPath := filepath.Join(worktree, localProjectFile)
		if err := os.Mkdir(projectPath, 0o755); err != nil {
			t.Fatal(err)
		}

		_, err := localProjectSourceFiles(projectPath)
		if err == nil || !strings.Contains(err.Error(), projectPath) || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("expected path-specific entrypoint directory error, got %v", err)
		}
	})

	t.Run("entrypoint symlink", func(t *testing.T) {
		worktree := t.TempDir()
		projectPath := filepath.Join(worktree, localProjectFile)
		writeTestFile(t, filepath.Join(worktree, "entry.go"), "package main\n")
		if err := os.Symlink("entry.go", projectPath); err != nil {
			t.Skipf("symlink creation is unavailable: %v", err)
		}

		_, err := localProjectSourceFiles(projectPath)
		if err == nil || !strings.Contains(err.Error(), projectPath) || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("expected path-specific entrypoint symlink error, got %v", err)
		}
	})
}

func TestLocalBuildKeyTracksCompanionSetNamesAndContents(t *testing.T) {
	worktree := t.TempDir()
	projectPath := filepath.Join(worktree, localProjectFile)
	companionPath := filepath.Join(worktree, "devflow_shared.go")
	writeTestFile(t, projectPath, "package main\n")

	entryOnlyKey := readComputedLocalBuildKey(t, projectPath)
	writeTestFile(t, companionPath, "package main\n\nconst value = 1\n")
	addedKey := readComputedLocalBuildKey(t, projectPath)
	assertDifferentKeys(t, entryOnlyKey, addedKey, "adding a companion")

	touchedAt := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(companionPath, touchedAt, touchedAt); err != nil {
		t.Fatal(err)
	}
	if got := readComputedLocalBuildKey(t, projectPath); got != addedKey {
		t.Fatalf("timestamp-only companion change altered build key: got %s want %s", got, addedKey)
	}

	writeTestFile(t, companionPath, "package main\n\nconst value = 2\n")
	editedKey := readComputedLocalBuildKey(t, projectPath)
	assertDifferentKeys(t, addedKey, editedKey, "editing a companion")

	renamedPath := filepath.Join(worktree, "devflow_backend.go")
	if err := os.Rename(companionPath, renamedPath); err != nil {
		t.Fatal(err)
	}
	renamedKey := readComputedLocalBuildKey(t, projectPath)
	assertDifferentKeys(t, editedKey, renamedKey, "renaming a companion")

	if err := os.Remove(renamedPath); err != nil {
		t.Fatal(err)
	}
	removedKey := readComputedLocalBuildKey(t, projectPath)
	assertDifferentKeys(t, renamedKey, removedKey, "removing a companion")
	if removedKey != entryOnlyKey {
		t.Fatalf("removing the only companion did not restore the entry-only key: got %s want %s", removedKey, entryOnlyKey)
	}

	writeTestFile(t, filepath.Join(worktree, "devflow_watch_test.go"), "excluded test v1\n")
	writeTestFile(t, filepath.Join(worktree, "backend.go"), "unrelated source v1\n")
	if got := readComputedLocalBuildKey(t, projectPath); got != removedKey {
		t.Fatalf("excluded sibling files altered build key: got %s want %s", got, removedKey)
	}
	writeTestFile(t, filepath.Join(worktree, "devflow_watch_test.go"), "excluded test v2\n")
	writeTestFile(t, filepath.Join(worktree, "backend.go"), "unrelated source v2\n")
	if got := readComputedLocalBuildKey(t, projectPath); got != removedKey {
		t.Fatalf("editing excluded sibling files altered build key: got %s want %s", got, removedKey)
	}
}

func TestBootstrapLoadsAndRebuildsMultiFileProject(t *testing.T) {
	worktree := t.TempDir()
	writeMultiFileLocalProject(t, worktree, "ci_initial")
	writeTestFile(t, filepath.Join(worktree, "devflow_watch_test.go"), "this is deliberately invalid Go\n")
	writeTestFile(t, filepath.Join(worktree, "backend.go"), "this is deliberately invalid Go\n")

	initialTargets := runBootstrapGraphTargets(t, worktree)
	assertStringSetContains(t, initialTargets, "frontend", "backend", "ci_initial")
	initialKey := readPublishedLocalBuildKey(t, worktree)
	assertGeneratedLocalProjectSources(t, worktree, []string{
		localProjectFile,
		"devflow_backend.go",
		"devflow_ci.go",
		"devflow_frontend.go",
		"devflow_shared.go",
	}, []string{"devflow_watch_test.go", "backend.go"})

	ciPath := filepath.Join(worktree, "devflow_ci.go")
	writeTestFile(t, ciPath, multiFileCISource("ci_edited"))
	editedTargets := runBootstrapGraphTargets(t, worktree)
	assertStringSetContains(t, editedTargets, "frontend", "backend", "ci_edited")
	if slices.Contains(editedTargets, "ci_initial") {
		t.Fatalf("companion edit left stale target in rebuilt graph: %v", editedTargets)
	}
	editedKey := readPublishedLocalBuildKey(t, worktree)
	assertDifferentKeys(t, initialKey, editedKey, "editing the CI companion")

	binaryPath := localProjectBinaryPathForTest(worktree)
	beforeTouch, err := os.Stat(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	touchedAt := time.Now().Add(3 * time.Second)
	if err := os.Chtimes(ciPath, touchedAt, touchedAt); err != nil {
		t.Fatal(err)
	}
	runBootstrapGraphTargets(t, worktree)
	afterTouch, err := os.Stat(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := readPublishedLocalBuildKey(t, worktree); got != editedKey {
		t.Fatalf("timestamp-only companion change altered published build key: got %s want %s", got, editedKey)
	}
	if !afterTouch.ModTime().Equal(beforeTouch.ModTime()) {
		t.Fatalf("timestamp-only companion change rebuilt binary: before=%s after=%s", beforeTouch.ModTime(), afterTouch.ModTime())
	}

	// Excluded files may be normal project tests or unrelated Go sources, even if
	// they would not compile as part of the generated runtime adapter package.
	writeTestFile(t, filepath.Join(worktree, "devflow_watch_test.go"), "still deliberately invalid Go\n")
	writeTestFile(t, filepath.Join(worktree, "backend.go"), "still deliberately invalid Go\n")
	runBootstrapGraphTargets(t, worktree)
	if got := readPublishedLocalBuildKey(t, worktree); got != editedKey {
		t.Fatalf("excluded source edit altered published build key: got %s want %s", got, editedKey)
	}

	addedPath := filepath.Join(worktree, "devflow_added.go")
	writeTestFile(t, addedPath, multiFileAddedTargetSource())
	addedTargets := runBootstrapGraphTargets(t, worktree)
	assertStringSetContains(t, addedTargets, "added")
	addedKey := readPublishedLocalBuildKey(t, worktree)
	assertDifferentKeys(t, editedKey, addedKey, "adding a companion")

	renamedPath := filepath.Join(worktree, "devflow_extra.go")
	if err := os.Rename(addedPath, renamedPath); err != nil {
		t.Fatal(err)
	}
	renamedTargets := runBootstrapGraphTargets(t, worktree)
	assertStringSetContains(t, renamedTargets, "added")
	renamedKey := readPublishedLocalBuildKey(t, worktree)
	assertDifferentKeys(t, addedKey, renamedKey, "renaming a companion")
	assertGeneratedLocalProjectSources(t, worktree, []string{"devflow_extra.go"}, []string{"devflow_added.go"})

	if err := os.Remove(renamedPath); err != nil {
		t.Fatal(err)
	}
	removedTargets := runBootstrapGraphTargets(t, worktree)
	if slices.Contains(removedTargets, "added") {
		t.Fatalf("removed companion left stale target in graph: %v", removedTargets)
	}
	removedKey := readPublishedLocalBuildKey(t, worktree)
	assertDifferentKeys(t, renamedKey, removedKey, "removing a companion")
	assertGeneratedLocalProjectSources(t, worktree, nil, []string{"devflow_extra.go", "devflow_added.go"})
}

func TestBootstrapFailedCompanionBuildPreservesPreviousBinary(t *testing.T) {
	worktree := t.TempDir()
	writeMultiFileLocalProject(t, worktree, "ci_working")
	runBootstrapGraphTargets(t, worktree)

	binaryPath := localProjectBinaryPathForTest(worktree)
	beforeBinary, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeKey := readPublishedLocalBuildKey(t, worktree)
	brokenPath := filepath.Join(worktree, "devflow_broken.go")
	writeTestFile(t, brokenPath, "package main\n\nfunc brokenCompanion(\n")

	output, err := runBootstrapCommand(t, worktree, "graph", "list", "--json")
	if err == nil {
		t.Fatalf("expected invalid companion build to fail, got %s", output)
	}
	if !strings.Contains(output, "devflow_broken.go") {
		t.Fatalf("compile failure did not identify companion path: %s", output)
	}
	afterBinary, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(beforeBinary, afterBinary) {
		t.Fatal("failed companion build replaced the previously working local binary")
	}
	if got := readPublishedLocalBuildKey(t, worktree); got != beforeKey {
		t.Fatalf("failed companion build replaced published key: got %s want %s", got, beforeKey)
	}

	cmd := exec.Command(binaryPath, "graph", "list", "--json", "--project", multiFileProjectName)
	cmd.Dir = worktree
	cmd.Env = withEnv(os.Environ(), envLocalExec, "1")
	directOutput, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("previous local binary is no longer runnable: %v\n%s", err, directOutput)
	}
	if !strings.Contains(string(directOutput), "ci_working") {
		t.Fatalf("previous local binary lost its working graph: %s", directOutput)
	}
}

func readComputedLocalBuildKey(t *testing.T, projectPath string) string {
	t.Helper()
	key, err := localBuildKey("", projectPath)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func readPublishedLocalBuildKey(t *testing.T, worktree string) string {
	t.Helper()
	data, err := os.ReadFile(localBuildKeyPath(localProjectBinaryPathForTest(worktree)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func assertDifferentKeys(t *testing.T, before, after, operation string) {
	t.Helper()
	if before == after {
		t.Fatalf("%s did not change local build key %s", operation, before)
	}
}

func runBootstrapGraphTargets(t *testing.T, worktree string) []string {
	t.Helper()
	output, err := runBootstrapCommand(t, worktree, "graph", "list", "--json")
	if err != nil {
		t.Fatalf("multi-file bootstrap failed: %v\n%s", err, output)
	}
	var payload struct {
		Targets []string `json:"targets"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		t.Fatalf("decode graph JSON %q: %v", output, err)
	}
	return payload.Targets
}

func assertStringSetContains(t *testing.T, got []string, want ...string) {
	t.Helper()
	for _, item := range want {
		if !slices.Contains(got, item) {
			t.Fatalf("missing %q from %v", item, got)
		}
	}
}

func assertGeneratedLocalProjectSources(t *testing.T, worktree string, included, excluded []string) {
	t.Helper()
	buildRoot := filepath.Join(worktree, ".devflow", "localbuild")
	entries, err := os.ReadDir(buildRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("expected one generated local-build directory in %s, got %v", buildRoot, entries)
	}
	buildDir := filepath.Join(buildRoot, entries[0].Name())
	for _, name := range included {
		info, err := os.Lstat(filepath.Join(buildDir, name))
		if err != nil {
			t.Fatalf("included source %s missing from generated module: %v", name, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("included source %s is not a regular generated source: %s", name, info.Mode())
		}
	}
	for _, name := range excluded {
		if _, err := os.Lstat(filepath.Join(buildDir, name)); !os.IsNotExist(err) {
			t.Fatalf("excluded source %s exists in generated module (err=%v)", name, err)
		}
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

const multiFileProjectName = "local-multi-file-project"

func writeMultiFileLocalProject(t *testing.T, worktree, ciTarget string) {
	t.Helper()
	sources := map[string]string{
		localProjectFile: `package main

import "github.com/benjaco/devflow/pkg/project"

func init() {
	project.Register(localMultiFileProject{})
}
`,
		"devflow_shared.go": `package main

import (
	"context"

	"github.com/benjaco/devflow/pkg/project"
)

type localMultiFileProject struct{}

var extraLocalTargets []project.Target

func (localMultiFileProject) Name() string { return "local-multi-file-project" }

func (localMultiFileProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	_ = worktree
	return project.InstanceConfig{Label: "multi-file"}, nil
}

func localNoopTask(name string) project.Task {
	return project.Task{
		Name: name,
		Kind: project.KindOnce,
		Run: func(context.Context, *project.Runtime) error { return nil },
	}
}
`,
		"devflow_frontend.go": `package main

import "github.com/benjaco/devflow/pkg/project"

func localFrontendTask() project.Task {
	return localNoopTask("frontend_noop")
}
`,
		"devflow_backend.go": `package main

import "github.com/benjaco/devflow/pkg/project"

func localBackendTask() project.Task {
	return localNoopTask("backend_noop")
}
`,
		"devflow_ci.go": multiFileCISource(ciTarget),
	}
	for name, source := range sources {
		writeTestFile(t, filepath.Join(worktree, name), source)
	}
}

func multiFileCISource(target string) string {
	return fmt.Sprintf(`package main

import "github.com/benjaco/devflow/pkg/project"

func (localMultiFileProject) Tasks() []project.Task {
	return []project.Task{localFrontendTask(), localBackendTask()}
}

func (localMultiFileProject) Targets() []project.Target {
	targets := []project.Target{
		{Name: "frontend", RootTasks: []string{"frontend_noop"}},
		{Name: "backend", RootTasks: []string{"backend_noop"}},
		{Name: %q, RootTasks: []string{"frontend_noop", "backend_noop"}},
	}
	return append(targets, extraLocalTargets...)
}
`, target)
}

func multiFileAddedTargetSource() string {
	return `package main

import "github.com/benjaco/devflow/pkg/project"

func init() {
	extraLocalTargets = append(extraLocalTargets, project.Target{Name: "added", RootTasks: []string{"frontend_noop"}})
}
`
}
