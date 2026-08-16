package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

type manifestTestProject struct {
	signature string
	calls     *atomic.Int32
}

func (p manifestTestProject) Name() string { return "manifest-test-project" }

func (p manifestTestProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{
		Label: "manifest-test",
		Env:   map[string]string{"CONFIG_SECRET": "configured-secret"},
	}, nil
}

func (p manifestTestProject) Tasks() []project.Task {
	return []project.Task{{
		Name:        "build",
		Kind:        project.KindOnce,
		Cache:       true,
		Signature:   p.signature,
		RequiredEnv: []string{"REMOTE_TOKEN"},
		Inputs: project.Inputs{
			Files: []string{"input.txt"},
			Env:   []string{"REMOTE_TOKEN"},
			Custom: []project.FingerprintFunc{func(context.Context, *project.Runtime) (string, error) {
				p.calls.Add(1)
				return "postgresql://user:semantic-secret@example.invalid/app", nil
			}},
		},
		Outputs: project.Outputs{Files: []string{"out.txt"}},
		Run: func(_ context.Context, rt *project.Runtime) error {
			data, err := os.ReadFile(rt.Abs("input.txt"))
			if err != nil {
				return err
			}
			return os.WriteFile(rt.Abs("out.txt"), data, 0o644)
		},
	}}
}

func (p manifestTestProject) Targets() []project.Target {
	return []project.Target{
		{Name: "build", RootTasks: []string{"build"}},
		{Name: "other", RootTasks: []string{"build"}},
	}
}

func TestCacheKeyManifestReusesSemanticFingerprintExactlyOnce(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMOTE_TOKEN", "environment-secret")
	calls := &atomic.Int32{}
	p := manifestTestProject{signature: "v1", calls: calls}
	eng, err := New(p, worktree)
	if err != nil {
		t.Fatal(err)
	}
	keyResult, manifest, err := eng.CacheKeyWithManifest(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("cache key semantic callback calls = %d, want 1", calls.Load())
	}
	manifestPath := filepath.Join(t.TempDir(), "cache-key-manifest.json")
	if err := WriteCacheKeyManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("manifest mode = %03o", info.Mode().Perm())
		}
	}
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"semantic-secret", "environment-secret", "configured-secret", "postgresql://"} {
		if strings.Contains(string(manifestData), secret) {
			t.Fatalf("secret %q leaked into manifest: %s", secret, manifestData)
		}
	}

	outcome, err := eng.Run(context.Background(), Request{
		Target:               "build",
		Worktree:             worktree,
		Mode:                 api.ModeCI,
		CacheKeyManifestPath: manifestPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("cache key plus manifest-backed run invoked callback %d times, want exactly 1", calls.Load())
	}
	t.Logf("semantic fingerprint callbacks across cache-key preflight and manifest-backed run: %d", calls.Load())
	if outcome.Result.CacheKeyManifest == nil || !outcome.Result.CacheKeyManifest.Validated || outcome.Result.CacheKeyManifest.ReusedComponents != 1 || !reflectStringSliceEqual(outcome.Result.CacheKeyManifest.ReusedTasks, []string{"build"}) {
		t.Fatalf("missing manifest reuse report: %+v", outcome.Result.CacheKeyManifest)
	}
	if len(outcome.Result.Nodes) != 1 || outcome.Result.Nodes[0].LastRunKey != keyResult.TaskKeys[0].Key {
		t.Fatalf("unchanged local inputs did not retain the planned key: plan=%+v run=%+v", keyResult, outcome.Result.Nodes)
	}
	timing := outcome.Result.Nodes[0].Cache
	if timing == nil || timing.ManifestValidationMs <= 0 || !reflectStringSliceEqual(timing.ManifestComponents, []string{"custom:0000"}) {
		t.Fatalf("manifest timing/source missing from node cache timing: %+v", timing)
	}
	encodedResult, err := json.Marshal(outcome.Result)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"semantic-secret", "environment-secret", "configured-secret"} {
		if strings.Contains(string(encodedResult), secret) {
			t.Fatalf("secret %q leaked into final JSON: %s", secret, encodedResult)
		}
	}
}

func TestCacheKeyThenRunWithoutManifestRetainsFingerprintEvaluation(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMOTE_TOKEN", "token")
	calls := &atomic.Int32{}
	eng, err := New(manifestTestProject{signature: "v1", calls: calls}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.CacheKey(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("preflight plus ordinary run callback calls = %d, want 2", calls.Load())
	}
}

func TestCacheKeyManifestRejectsTargetGraphAndEnvironmentChanges(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMOTE_TOKEN", "first-secret")
	calls := &atomic.Int32{}
	baseProject := manifestTestProject{signature: "v1", calls: calls}
	eng, err := New(baseProject, worktree)
	if err != nil {
		t.Fatal(err)
	}
	_, manifest, err := eng.CacheKeyWithManifest(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	if err := WriteCacheKeyManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		project manifestTestProject
		target  string
		prepare func()
		want    string
	}{
		{name: "target", project: baseProject, target: "other", prepare: func() {}, want: "another target"},
		{name: "graph", project: manifestTestProject{signature: "v2", calls: calls}, target: "build", prepare: func() {}, want: "graph or configuration changed"},
		{name: "environment", project: baseProject, target: "build", prepare: func() { t.Setenv("REMOTE_TOKEN", "second-secret") }, want: "environment changed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.prepare()
			candidate, err := New(test.project, worktree)
			if err != nil {
				t.Fatal(err)
			}
			outcome, runErr := candidate.Run(context.Background(), Request{Target: test.target, Worktree: worktree, Mode: api.ModeCI, CacheKeyManifestPath: manifestPath})
			if runErr == nil || outcome == nil || outcome.Result.Success || !strings.Contains(runErr.Error(), test.want) {
				t.Fatalf("unexpected manifest rejection: outcome=%+v err=%v", outcome, runErr)
			}
			var manifestErr *CacheKeyManifestError
			if !errors.As(runErr, &manifestErr) || outcome.Result.CacheKeyManifest == nil || outcome.Result.CacheKeyManifest.Validated || !strings.Contains(outcome.Result.CacheKeyManifest.Error, test.want) {
				t.Fatalf("manifest rejection was not structured: outcome=%+v err=%v", outcome, runErr)
			}
		})
	}
}

func TestCacheKeyManifestRecomputesChangedLocalAndGeneratedInputs(t *testing.T) {
	isolateEngineUserCache(t)
	t.Run("local input", func(t *testing.T) {
		worktree := t.TempDir()
		input := filepath.Join(worktree, "input.txt")
		if err := os.WriteFile(input, []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("REMOTE_TOKEN", "token")
		calls := &atomic.Int32{}
		eng, err := New(manifestTestProject{signature: "v1", calls: calls}, worktree)
		if err != nil {
			t.Fatal(err)
		}
		keyResult, manifest, err := eng.CacheKeyWithManifest(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
		if err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(t.TempDir(), "manifest.json")
		if err := WriteCacheKeyManifest(manifestPath, manifest); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(input, []byte("after"), 0o644); err != nil {
			t.Fatal(err)
		}
		outcome, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI, CacheKeyManifestPath: manifestPath})
		if err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 1 || outcome.Result.Nodes[0].LastRunKey == keyResult.TaskKeys[0].Key || !outcome.Result.Nodes[0].Cache.LocalInputsChangedFromManifest || !reflectStringSliceEqual(outcome.Result.CacheKeyManifest.LocalInputChangedTasks, []string{"build"}) {
			t.Fatalf("local input was not safely recomputed: calls=%d plan=%+v run=%+v usage=%+v", calls.Load(), keyResult, outcome.Result.Nodes, outcome.Result.CacheKeyManifest)
		}
	})

	t.Run("upstream generated input", func(t *testing.T) {
		worktree := t.TempDir()
		calls := &atomic.Int32{}
		p := generatedManifestProject{calls: calls}
		eng, err := New(p, worktree)
		if err != nil {
			t.Fatal(err)
		}
		keyResult, manifest, err := eng.CacheKeyWithManifest(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
		if err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(t.TempDir(), "manifest.json")
		if err := WriteCacheKeyManifest(manifestPath, manifest); err != nil {
			t.Fatal(err)
		}
		outcome, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI, CacheKeyManifestPath: manifestPath})
		if err != nil {
			t.Fatal(err)
		}
		plannedDownstream := keyResult.TaskKeys[1].Key
		var downstream api.NodeStatus
		for _, node := range outcome.Result.Nodes {
			if node.Name == "consume" {
				downstream = node
			}
		}
		if calls.Load() != 1 || downstream.LastRunKey == plannedDownstream || downstream.Cache == nil || !downstream.Cache.LocalInputsChangedFromManifest {
			t.Fatalf("generated downstream key was blindly reused: calls=%d planned=%s node=%+v", calls.Load(), plannedDownstream, downstream)
		}
	})
}

func TestCacheKeyManifestRejectsCorruptModifiedAndExpiredFiles(t *testing.T) {
	isolateEngineUserCache(t)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "input.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMOTE_TOKEN", "token")
	calls := &atomic.Int32{}
	eng, err := New(manifestTestProject{signature: "v1", calls: calls}, worktree)
	if err != nil {
		t.Fatal(err)
	}
	_, manifest, err := eng.CacheKeyWithManifest(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("corrupt", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "manifest.json")
		if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertManifestRejected(t, eng, worktree, path, "invalid JSON")
	})
	t.Run("modified", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "manifest.json")
		modified := *manifest
		modified.AggregateKey = strings.Repeat("0", len(modified.AggregateKey))
		if err := WriteCacheKeyManifest(path, &modified); err != nil {
			t.Fatal(err)
		}
		assertManifestRejected(t, eng, worktree, path, "integrity")
	})
	t.Run("expired", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "manifest.json")
		expired := *manifest
		created := time.Now().UTC().Add(-CacheKeyManifestMaxAge - time.Minute)
		expired.CreatedAt = created.Format(time.RFC3339Nano)
		expired.ExpiresAt = created.Add(CacheKeyManifestMaxAge).Format(time.RFC3339Nano)
		expired.Integrity = cacheKeyManifestIntegrity(expired)
		if err := WriteCacheKeyManifest(path, &expired); err != nil {
			t.Fatal(err)
		}
		assertManifestRejected(t, eng, worktree, path, "expired")
	})
}

type generatedManifestProject struct {
	calls *atomic.Int32
}

func (generatedManifestProject) Name() string { return "generated-manifest-project" }
func (generatedManifestProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{Label: "generated-manifest"}, nil
}
func (p generatedManifestProject) Tasks() []project.Task {
	return []project.Task{
		{
			Name:    "generate",
			Kind:    project.KindOnce,
			Cache:   true,
			Outputs: project.Outputs{Files: []string{"generated.txt"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				return os.WriteFile(rt.Abs("generated.txt"), []byte("generated"), 0o644)
			},
		},
		{
			Name:  "consume",
			Kind:  project.KindOnce,
			Cache: true,
			Deps:  []string{"generate"},
			Inputs: project.Inputs{
				Files: []string{"generated.txt"},
				Custom: []project.FingerprintFunc{func(context.Context, *project.Runtime) (string, error) {
					p.calls.Add(1)
					return "remote-state", nil
				}},
			},
			Outputs: project.Outputs{Files: []string{"consumed.txt"}},
			Run: func(_ context.Context, rt *project.Runtime) error {
				data, err := os.ReadFile(rt.Abs("generated.txt"))
				if err != nil {
					return err
				}
				return os.WriteFile(rt.Abs("consumed.txt"), data, 0o644)
			},
		},
	}
}
func (generatedManifestProject) Targets() []project.Target {
	return []project.Target{{Name: "build", RootTasks: []string{"consume"}}}
}

func assertManifestRejected(t *testing.T, eng *Engine, worktree, path, reason string) {
	t.Helper()
	outcome, err := eng.Run(context.Background(), Request{Target: "build", Worktree: worktree, Mode: api.ModeCI, CacheKeyManifestPath: path})
	if err == nil || outcome == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("manifest was not rejected for %s: outcome=%+v err=%v", reason, outcome, err)
	}
}

func reflectStringSliceEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
