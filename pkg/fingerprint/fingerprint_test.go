package fingerprint

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"devflow/pkg/project"
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
