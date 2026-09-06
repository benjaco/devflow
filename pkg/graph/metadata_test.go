package graph

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/project"
)

func metadataFixture(t *testing.T) *Graph {
	t.Helper()
	hook := func(context.Context, *project.Runtime) error {
		t.Fatal("metadata executed an adapter hook")
		return nil
	}
	fingerprint := func(context.Context, *project.Runtime) (string, error) {
		t.Fatal("metadata evaluated a fingerprint")
		return "", nil
	}
	filter := project.ContentFilter("declarations:v1", func(context.Context, *project.Runtime, project.FileContent) ([]byte, error) {
		t.Fatal("metadata evaluated a content filter")
		return nil, nil
	})
	g, err := New([]project.Task{
		{
			Name: "z-service", Kind: project.KindDebugService, Deps: []string{"build"},
			RequiredCLIs: []string{"go"}, RequiredEnv: []string{"DATABASE_URL"},
			Inputs: project.Inputs{
				Paths: []string{"src"}, Files: []string{"go.mod"}, Dirs: []string{"assets"}, Globs: []string{"**/*.go"},
				Filtered: []project.FilteredInput{project.Filtered("schema.go", filter)}, Env: []string{"GOFLAGS"}, Ignore: []string{"vendor"},
				Custom: []project.FingerprintFunc{fingerprint},
			},
			Outputs:   project.Outputs{Paths: []string{"dist"}, Files: []string{"app"}, Dirs: []string{"generated"}},
			Purposes:  []project.Purpose{project.PurposeBuild},
			Effects:   &project.Effects{Writes: []string{"generated/**"}, Touches: []string{"database"}, Invalidates: []string{"build"}, Resources: []project.ResourceUse{{Name: "database", Access: project.ResourceRead}}},
			BeforeRun: hook, Run: hook, Ready: project.ReadyFunc(hook), AfterReady: hook, CacheKeyOverride: project.CacheKeyFunc(fingerprint),
			ReadyTimeout: time.Second, Restart: project.RestartOnInputChange, WatchRestartOnServiceDeps: true, AllowInWatch: true,
			Tags: []string{"backend"}, Description: "Application", Signature: "COMMAND_ENV_SECRET", Debug: &project.DebugConfig{Host: "DEBUG_HOST_SECRET"},
		},
		{Name: "unknown", Kind: project.KindOnce},
		{Name: "build", Kind: project.KindOnce, Cache: true, Outputs: project.Outputs{Paths: []string{"dist"}}, Effects: &project.Effects{}},
		{Name: "install", Kind: project.KindOnce, Stamp: true},
	}, []project.Target{
		{Name: "up", RootTasks: []string{"z-service"}},
		{Name: "verify", RootTasks: []string{"build"}, RequiredCLIs: []string{"go"}, RequiredEnv: []string{"CI"}, Description: "Verification", Verification: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestMetadataProjectsSafeDeclarationsWithoutExecutingCallbacks(t *testing.T) {
	g := metadataFixture(t)
	metadata := g.Metadata()
	if got := []string{metadata.Tasks[0].Name, metadata.Tasks[1].Name, metadata.Tasks[2].Name, metadata.Tasks[3].Name}; !reflect.DeepEqual(got, []string{"build", "install", "unknown", "z-service"}) {
		t.Fatalf("task order = %v", got)
	}
	if got := []string{metadata.Targets[0].Name, metadata.Targets[1].Name}; !reflect.DeepEqual(got, []string{"up", "verify"}) {
		t.Fatalf("target order = %v", got)
	}
	service := metadata.Tasks[3]
	wantInputs := InputMetadata{
		Paths: []string{"src"}, Files: []string{"go.mod"}, Dirs: []string{"assets"}, Globs: []string{"**/*.go"},
		Filtered: []FilteredInputMetadata{{Path: "schema.go", FilterSignature: "declarations:v1"}},
		Env:      []string{"GOFLAGS"}, Ignore: []string{"vendor"}, CustomFingerprintCount: 1,
	}
	if !reflect.DeepEqual(service.Inputs, wantInputs) {
		t.Fatalf("inputs = %+v", service.Inputs)
	}
	if !reflect.DeepEqual(service.Outputs, OutputMetadata{Paths: []string{"dist"}, Files: []string{"app"}, Dirs: []string{"generated"}}) {
		t.Fatalf("outputs = %+v", service.Outputs)
	}
	if !service.HasBeforeRun || !service.HasRun || !service.HasReady || !service.HasAfterReady || !service.HasCacheKeyOverride || !service.HasDebug {
		t.Fatalf("missing hook presence: %+v", service)
	}
	if service.ReadyTimeout != time.Second || service.Restart != project.RestartOnInputChange || !service.WatchRestartOnServiceDeps || !service.AllowInWatch || service.Cache || service.Stamp || !metadata.Tasks[0].Cache || !metadata.Tasks[1].Stamp {
		t.Fatalf("execution declarations = %+v", metadata.Tasks)
	}
	if !reflect.DeepEqual(service.Deps, []string{"build"}) || !reflect.DeepEqual(service.RequiredCLIs, []string{"go"}) || !reflect.DeepEqual(service.RequiredEnv, []string{"DATABASE_URL"}) || service.Description != "Application" || service.Tags[0] != "backend" || service.Purposes[0] != project.PurposeBuild {
		t.Fatalf("task declarations = %+v", service)
	}
	if !reflect.DeepEqual(metadata.Targets[1], TargetMetadata{Name: "verify", RootTasks: []string{"build"}, RequiredCLIs: []string{"go"}, RequiredEnv: []string{"CI"}, Description: "Verification", Verification: true}) {
		t.Fatalf("verification target = %+v", metadata.Targets[1])
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"COMMAND_ENV_SECRET", "DEBUG_HOST_SECRET", `"signature"`, `"cacheKeyOverride"`, `"beforeRun"`} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("metadata leaked %q: %s", secret, data)
		}
	}
	var decoded struct {
		Tasks []map[string]json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded.Tasks[0]["effects"]) != "{}" || string(decoded.Tasks[2]["effects"]) != "null" {
		t.Fatalf("effects must distinguish unknown and declared empty: %s", data)
	}
}

func TestMetadataDoesNotShareMutableDeclarations(t *testing.T) {
	g := metadataFixture(t)
	before, err := json.Marshal(g.Metadata())
	if err != nil {
		t.Fatal(err)
	}
	metadata := g.Metadata()
	mutateMetadataStrings(reflect.ValueOf(&metadata).Elem())
	after, err := json.Marshal(g.Metadata())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("metadata mutation changed adapter declarations:\nbefore %s\nafter %s", before, after)
	}
}

func mutateMetadataStrings(value reflect.Value) {
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			mutateMetadataStrings(value.Elem())
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			mutateMetadataStrings(value.Field(i))
		}
	case reflect.Slice:
		for i := 0; i < value.Len(); i++ {
			mutateMetadataStrings(value.Index(i))
		}
	case reflect.String:
		value.SetString("changed")
	}
}

func TestMetadataForTargetAndDigest(t *testing.T) {
	g := metadataFixture(t)
	selected, err := g.MetadataForTarget("up")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Targets) != 1 || selected.Targets[0].Name != "up" || len(selected.Tasks) != 2 || selected.Tasks[0].Name != "build" || selected.Tasks[1].Name != "z-service" {
		t.Fatalf("target projection = %+v", selected)
	}
	if _, err := g.MetadataForTarget("absent"); err == nil {
		t.Fatal("missing target accepted")
	}
	first := g.Metadata().Digest
	if len(first) != 64 || first == selected.Digest || first != g.Metadata().Digest {
		t.Fatalf("unstable or unscoped digest %q", first)
	}
	service := g.Tasks["z-service"]
	service.Signature = "other private signature"
	service.Debug.Host = "other private host"
	g.Tasks[service.Name] = service
	if got := g.Metadata().Digest; got != first {
		t.Fatalf("private runtime details changed metadata digest: %q != %q", got, first)
	}
	service.Purposes = []project.Purpose{project.PurposeTest}
	g.Tasks[service.Name] = service
	if got := g.Metadata().Digest; got == first {
		t.Fatal("changed declaration did not change metadata digest")
	}
}
