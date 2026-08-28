package fingerprint

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/project"
)

func TestHashDirDeterministic(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum1, err := HashDir(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	sum2, err := HashDir(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum1 != sum2 {
		t.Fatalf("dir hash not deterministic: %s != %s", sum1, sum2)
	}
}

func TestTaskKeyIgnoresOrder(t *testing.T) {
	task := project.Task{Name: "gen", Kind: project.KindOnce}
	key1, err := TaskKey(TaskKeyInput{
		Task:               task,
		DepKeys:            []string{"b", "a"},
		InputHashes:        []string{"2", "1"},
		EnvValues:          []string{"Y=2", "X=1"},
		CustomFingerprints: []string{"two", "one"},
	})
	if err != nil {
		t.Fatal(err)
	}
	key2, err := TaskKey(TaskKeyInput{
		Task:               task,
		DepKeys:            []string{"a", "b"},
		InputHashes:        []string{"1", "2"},
		EnvValues:          []string{"X=1", "Y=2"},
		CustomFingerprints: []string{"one", "two"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if key1 != key2 {
		t.Fatalf("task key changed with ordering: %s != %s", key1, key2)
	}
}

func TestCollectTaskInputsIncludesEnvAndCustom(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := &project.Runtime{
		Worktree: root,
		Env:      map[string]string{"FOO": "bar"},
	}
	task := project.Task{
		Name:   "gen",
		Kind:   project.KindOnce,
		Inputs: project.Inputs{Files: []string{"input.txt"}, Env: []string{"FOO"}, Custom: []project.FingerprintFunc{func(ctx context.Context, rt *project.Runtime) (string, error) { return "custom", nil }}},
	}
	hashes, envValues, custom, err := CollectTaskInputs(context.Background(), root, task, rt)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 1 || len(envValues) != 1 || len(custom) != 1 {
		t.Fatalf("unexpected inputs: hashes=%v env=%v custom=%v", hashes, envValues, custom)
	}
}

func TestTaskKeyOverrideIsSaltedByTaskName(t *testing.T) {
	first, err := TaskKey(TaskKeyInput{
		Task:     project.Task{Name: "gen_a", Kind: project.KindOnce, Cache: true, CacheKeyOverride: func(ctx context.Context, rt *project.Runtime) (string, error) { return "semantic", nil }},
		Override: "semantic",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := TaskKey(TaskKeyInput{
		Task:     project.Task{Name: "gen_b", Kind: project.KindOnce, Cache: true, CacheKeyOverride: func(ctx context.Context, rt *project.Runtime) (string, error) { return "semantic", nil }},
		Override: "semantic",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("expected override keys to differ across task names: %s", first)
	}
}

func TestTaskKeyOverrideRejectsEmptyValue(t *testing.T) {
	_, err := TaskKey(TaskKeyInput{
		Task:     project.Task{Name: "gen", Kind: project.KindOnce, Cache: true, CacheKeyOverride: func(ctx context.Context, rt *project.Runtime) (string, error) { return "", nil }},
		Override: "",
	})
	if err == nil {
		t.Fatal("expected empty override value to fail")
	}
}

func TestTaskKeyChangesWhenDependencyKeysChange(t *testing.T) {
	task := project.Task{Name: "gen", Kind: project.KindOnce}
	first, err := TaskKey(TaskKeyInput{Task: task, DepKeys: []string{"dep-a"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := TaskKey(TaskKeyInput{Task: task, DepKeys: []string{"dep-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("expected task key to change when dependency keys change")
	}
}

func TestTaskKeyChangesWhenInputHashesChange(t *testing.T) {
	task := project.Task{Name: "gen", Kind: project.KindOnce}
	first, err := TaskKey(TaskKeyInput{Task: task, InputHashes: []string{"file:a"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := TaskKey(TaskKeyInput{Task: task, InputHashes: []string{"file:b"}})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("expected task key to change when input hashes change")
	}
}

func TestTaskKeyChangesWhenEnvValuesChange(t *testing.T) {
	task := project.Task{Name: "gen", Kind: project.KindOnce}
	first, err := TaskKey(TaskKeyInput{Task: task, EnvValues: []string{"FOO=a"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := TaskKey(TaskKeyInput{Task: task, EnvValues: []string{"FOO=b"}})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("expected task key to change when env values change")
	}
}

func TestTaskSignatureTracksLifecycleHooks(t *testing.T) {
	base := project.Task{Name: "app", Kind: project.KindService}
	baseSignature, err := TaskSignature(base)
	if err != nil {
		t.Fatal(err)
	}
	withBefore := base
	withBefore.BeforeRun = func(context.Context, *project.Runtime) error { return nil }
	beforeSignature, err := TaskSignature(withBefore)
	if err != nil {
		t.Fatal(err)
	}
	if baseSignature == beforeSignature {
		t.Fatal("BeforeRun did not change task signature")
	}
	withAfter := base
	withAfter.AfterReady = func(context.Context, *project.Runtime) error { return nil }
	afterSignature, err := TaskSignature(withAfter)
	if err != nil {
		t.Fatal(err)
	}
	if baseSignature == afterSignature || beforeSignature == afterSignature {
		t.Fatal("AfterReady did not produce a distinct task signature")
	}
}

func TestTaskKeyChangesWhenCustomFingerprintsChange(t *testing.T) {
	task := project.Task{Name: "gen", Kind: project.KindOnce}
	first, err := TaskKey(TaskKeyInput{Task: task, CustomFingerprints: []string{"semantic-a"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := TaskKey(TaskKeyInput{Task: task, CustomFingerprints: []string{"semantic-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("expected task key to change when custom fingerprints change")
	}
}

func TestTaskKeySupportsCustomFingerprintFunctions(t *testing.T) {
	task := project.Task{
		Name: "gen",
		Kind: project.KindOnce,
		Inputs: project.Inputs{Custom: []project.FingerprintFunc{
			func(context.Context, *project.Runtime) (string, error) { return "semantic", nil },
		}},
	}
	if _, err := TaskKey(TaskKeyInput{Task: task, CustomFingerprints: []string{"semantic"}}); err != nil {
		t.Fatalf("custom fingerprint function made task signature invalid: %v", err)
	}
}

func TestTaskSignatureDoesNotReorderTaskDefinition(t *testing.T) {
	task := project.Task{
		Name: "gen",
		Inputs: project.Inputs{
			Paths: []string{"z", "a"},
			Filtered: []project.FilteredInput{
				project.Filtered("z.go", project.LinesStartingWith("z")),
				project.Filtered("a.go", project.LinesStartingWith("a")),
			},
		},
		Outputs: project.Outputs{Paths: []string{"z.out", "a.out"}},
	}
	wantPaths := append([]string(nil), task.Inputs.Paths...)
	wantFiltered := []string{"z.go:lines-starting-with:[\"z\"]", "a.go:lines-starting-with:[\"a\"]"}
	wantOutputs := append([]string(nil), task.Outputs.Paths...)
	if _, err := TaskSignature(task); err != nil {
		t.Fatal(err)
	}
	gotFiltered := make([]string, 0, len(task.Inputs.Filtered))
	for _, input := range task.Inputs.Filtered {
		gotFiltered = append(gotFiltered, input.Path+":"+input.Filter.Signature)
	}
	if !reflect.DeepEqual(task.Inputs.Paths, wantPaths) ||
		!reflect.DeepEqual(gotFiltered, wantFiltered) ||
		!reflect.DeepEqual(task.Outputs.Paths, wantOutputs) {
		t.Fatalf("task signature mutated task definition: inputs=%v filtered=%v outputs=%v", task.Inputs.Paths, task.Inputs.Filtered, task.Outputs.Paths)
	}
}

func TestTaskKeyChangesWhenTaskDefinitionChanges(t *testing.T) {
	first, err := TaskKey(TaskKeyInput{
		Task: project.Task{
			Name:      "gen",
			Kind:      project.KindOnce,
			Signature: "v1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := TaskKey(TaskKeyInput{
		Task: project.Task{
			Name:      "gen",
			Kind:      project.KindOnce,
			Signature: "v2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("expected task key to change when task definition changes")
	}
}

func TestCollectTaskInputsHashChangesWhenFileChanges(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := &project.Runtime{
		Worktree: root,
		Env:      map[string]string{},
	}
	task := project.Task{
		Name:   "gen",
		Kind:   project.KindOnce,
		Inputs: project.Inputs{Files: []string{"input.txt"}},
	}
	first, _, _, err := CollectTaskInputs(context.Background(), root, task, rt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("goodbye"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, _, _, err := CollectTaskInputs(context.Background(), root, task, rt)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("unexpected input hash counts: %v vs %v", first, second)
	}
	if first[0] == second[0] {
		t.Fatalf("expected collected input hash to change after file edit: %v vs %v", first, second)
	}
}

func TestCollectTaskInputsSupportsPathAndGlobInputs(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "package.json", `{"name":"demo"}`)
	writeTestFile(t, root, "sql/users.sql", "select 1;")
	writeTestFile(t, root, "sql/nested/rides.sql", "select 2;")
	writeTestFile(t, root, "sql/nested/rides.go", "package sql")

	rt := &project.Runtime{Worktree: root, Env: map[string]string{}}
	task := project.Task{
		Name: "codegen",
		Inputs: project.Inputs{
			Paths: []string{"package.json"},
			Globs: []string{"sql/**/*.sql"},
		},
	}
	first, _, _, err := CollectTaskInputs(context.Background(), root, task, rt)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "sql/nested/rides.sql", "select 3;")
	second, _, _, err := CollectTaskInputs(context.Background(), root, task, rt)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(first, second) {
		t.Fatalf("expected glob input hash to change after matching file edit")
	}
	writeTestFile(t, root, "sql/nested/rides.go", "package sql\n")
	third, _, _, err := CollectTaskInputs(context.Background(), root, task, rt)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second, third) {
		t.Fatalf("non-matching glob file should not change inputs\nsecond: %v\n third: %v", second, third)
	}
}

func TestCollectTaskInputsFilteredGoSemanticContent(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "internal/api/users.go", `package api

// @Summary List users
func ListUsers() {
	println("first")
}

// User is returned by the API.
type User struct {
	ID int
}
`)
	rt := &project.Runtime{Worktree: root, Env: map[string]string{}}
	filter := project.CombineContentFilters(
		project.GoCommentLinesStartingWith("@"),
		project.GoStructDeclarations(),
	)
	task := project.Task{
		Name: "swagger",
		Kind: project.KindOnce,
		Inputs: project.Inputs{
			Filtered: []project.FilteredInput{
				project.Filtered(project.Glob("internal/**/*.go"), filter),
			},
		},
	}
	first, _, _, err := CollectTaskInputs(context.Background(), root, task, rt)
	if err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, root, "internal/api/users.go", `package api

// @Summary List users
func ListUsers() {
	println("second")
}

// User is returned by the API.
type User struct {
	ID int
}
`)
	second, _, _, err := CollectTaskInputs(context.Background(), root, task, rt)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("irrelevant function body edit changed filtered input hash\nfirst: %v\nsecond: %v", first, second)
	}

	writeTestFile(t, root, "internal/api/users.go", `package api

// @Summary Search users
func ListUsers() {
	println("second")
}

// User is returned by the API.
type User struct {
	ID int
}
`)
	annotationChanged, _, _, err := CollectTaskInputs(context.Background(), root, task, rt)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(second, annotationChanged) {
		t.Fatal("expected @ comment edit to change filtered input hash")
	}

	writeTestFile(t, root, "internal/api/users.go", `package api

// @Summary Search users
func ListUsers() {
	println("second")
}

// User includes API-visible docs.
type User struct {
	ID int
}
`)
	docChanged, _, _, err := CollectTaskInputs(context.Background(), root, task, rt)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(annotationChanged, docChanged) {
		t.Fatal("expected struct doc comment edit to change filtered input hash")
	}

	writeTestFile(t, root, "internal/api/users.go", `package api

// @Summary Search users
func ListUsers() {
	println("second")
}

// User includes API-visible docs.
type User struct {
	ID int
	Email string
}
`)
	structChanged, _, _, err := CollectTaskInputs(context.Background(), root, task, rt)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(docChanged, structChanged) {
		t.Fatal("expected struct field edit to change filtered input hash")
	}
}

func TestCollectTaskInputsWithCacheReusesFilteredFileHash(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.go")
	mustWriteFingerprintTestFile(t, path, "package demo\n")
	rt := &project.Runtime{Worktree: root, Env: map[string]string{}}
	calls := 0
	filter := project.ContentFilter("stable-filter:v1", func(ctx context.Context, rt *project.Runtime, file project.FileContent) ([]byte, error) {
		_ = ctx
		_ = rt
		_ = file
		calls++
		return []byte("stable\n"), nil
	})
	task := project.Task{
		Name: "filtered",
		Kind: project.KindOnce,
		Inputs: project.Inputs{
			Filtered: []project.FilteredInput{project.Filtered("input.go", filter)},
		},
	}
	cache := NewFilteredContentCache()
	first, _, _, err := CollectTaskInputsWithCache(context.Background(), root, task, rt, cache)
	if err != nil {
		t.Fatal(err)
	}
	second, _, _, err := CollectTaskInputsWithCache(context.Background(), root, task, rt, cache)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected unchanged file to reuse filtered hash, filter calls=%d", calls)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("expected cached filtered inputs to match\nfirst: %v\nsecond: %v", first, second)
	}

	mustWriteFingerprintTestFile(t, path, "package demo\nfunc changed() {}\n")
	modTime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	third, _, _, err := CollectTaskInputsWithCache(context.Background(), root, task, rt, cache)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected changed file to be filtered again, filter calls=%d", calls)
	}
	if !reflect.DeepEqual(second, third) {
		t.Fatalf("stable filtered output should keep same hash after raw file edit\nsecond: %v\nthird: %v", second, third)
	}
}

func writeTestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectTaskInputsIgnoresRootAndDirRelativePatterns(t *testing.T) {
	root := t.TempDir()
	mustWriteFingerprintTestFile(t, filepath.Join(root, "internal", "storage", "repo.go"), "repo")
	mustWriteFingerprintTestFile(t, filepath.Join(root, "internal", "storage", "sqlc", "users.sql.go"), "generated")
	rt := &project.Runtime{Worktree: root, Env: map[string]string{}}
	rootRelativeTask := project.Task{
		Name:   "root-ignore",
		Kind:   project.KindOnce,
		Inputs: project.Inputs{Dirs: []string{"internal/storage"}, Ignore: []string{"internal/storage/sqlc"}},
	}
	dirRelativeTask := project.Task{
		Name:   "dir-ignore",
		Kind:   project.KindOnce,
		Inputs: project.Inputs{Dirs: []string{"internal/storage"}, Ignore: []string{"sqlc"}},
	}
	first, _, _, err := CollectTaskInputs(context.Background(), root, rootRelativeTask, rt)
	if err != nil {
		t.Fatal(err)
	}
	second, _, _, err := CollectTaskInputs(context.Background(), root, dirRelativeTask, rt)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 || first[0] != second[0] {
		t.Fatalf("expected root and dir relative ignores to hash the same inputs: %v vs %v", first, second)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "storage", "sqlc", "users.sql.go"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _, _, err := CollectTaskInputs(context.Background(), root, rootRelativeTask, rt)
	if err != nil {
		t.Fatal(err)
	}
	if first[0] != after[0] {
		t.Fatalf("ignored generated file changed hash: %v vs %v", first, after)
	}
}

func TestCollectTaskInputsIgnoreCanSuppressExplicitFile(t *testing.T) {
	root := t.TempDir()
	mustWriteFingerprintTestFile(t, filepath.Join(root, "schema.json"), "schema")
	rt := &project.Runtime{Worktree: root, Env: map[string]string{}}
	task := project.Task{
		Name:   "explicit",
		Kind:   project.KindOnce,
		Inputs: project.Inputs{Files: []string{"schema.json"}, Ignore: []string{"schema.json"}},
	}
	hashes, _, _, err := CollectTaskInputs(context.Background(), root, task, rt)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 0 {
		t.Fatalf("expected ignored explicit file to be skipped, got %v", hashes)
	}
}

func mustWriteFingerprintTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
