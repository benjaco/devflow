package database

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"devflow/pkg/api"
)

func TestDesiredBuildsDedicatedInstanceIdentity(t *testing.T) {
	mgr := New()
	db := mgr.Desired("abc123", Config{
		HostPort:     55432,
		Database:     "app_wt_abc123",
		User:         "devflow",
		Password:     "secret",
		SnapshotRoot: "/tmp/snapshots",
	})
	if db.ContainerName != "devflow-pg-abc123" {
		t.Fatalf("unexpected container name: %q", db.ContainerName)
	}
	if db.VolumeName != "devflow-pgdata-abc123" {
		t.Fatalf("unexpected volume name: %q", db.VolumeName)
	}
	if db.Port != 55432 {
		t.Fatalf("unexpected port: %d", db.Port)
	}
	if !strings.Contains(db.URL, "@127.0.0.1:55432/app_wt_abc123") {
		t.Fatalf("unexpected database URL: %q", db.URL)
	}
}

func TestEnsureRuntimeCreatesVolumeAndContainer(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):               {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "volume", "create", "devflow-pgdata-abc"):                {},
			key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", "postgres:16.3"): {},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         "postgres:16.3",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker volume create devflow-pgdata-abc") {
		t.Fatal("expected docker volume create")
	}
	if !runner.sawPrefix("docker run -d --name devflow-pg-abc") {
		t.Fatal("expected docker run for container start")
	}
}

func TestSnapshotWritesManifestAndArchiveCommand(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "stop", "-t", "10", "devflow-pg-abc"): {},
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/from", "-v", filepath.Join(root, "schema_v1")+":/to", DefaultSidecarImage, "sh", "-c", "cd /from && tar czf /to/volume.tgz ."): {},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		User:          "devflow",
		Port:          55432,
		Image:         "postgres:16.3",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		SnapshotRoot:  root,
	}
	manifest, err := mgr.Snapshot(context.Background(), db, "schema_v1")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Key != "schema_v1" {
		t.Fatalf("unexpected snapshot key: %q", manifest.Key)
	}
	if _, err := os.Stat(filepath.Join(root, "schema_v1", "manifest.json")); err != nil {
		t.Fatalf("expected manifest to exist: %v", err)
	}
	if !runner.sawPrefix("docker stop -t 10 devflow-pg-abc") {
		t.Fatal("expected docker stop before snapshot")
	}
}

func TestRestoreSnapshotRecreatesVolumeAndUntars(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "schema_v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := SnapshotManifest{
		Version:       1,
		Key:           "schema_v1",
		Image:         "postgres:16.3",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		Database:      "app_wt_abc",
		User:          "devflow",
		Port:          55432,
		ArchivePath:   filepath.Join(root, "schema_v1", "volume.tgz"),
	}
	if err := os.WriteFile(filepath.Join(root, "schema_v1", "volume.tgz"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := jsonWrite(filepath.Join(root, "schema_v1", "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "rm", "-f", "devflow-pg-abc"):               {},
			key("docker", "volume", "rm", "-f", "devflow-pgdata-abc"): {},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):  {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "volume", "create", "devflow-pgdata-abc"):   {},
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/to", "-v", filepath.Join(root, "schema_v1")+":/from", DefaultSidecarImage, "sh", "-c", "cd /to && tar xzf /from/volume.tgz"): {},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		SnapshotRoot:  root,
	}
	got, err := mgr.RestoreSnapshot(context.Background(), db, "schema_v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "schema_v1" {
		t.Fatalf("unexpected restored manifest key: %q", got.Key)
	}
	if !runner.sawPrefix("docker volume create devflow-pgdata-abc") {
		t.Fatal("expected docker volume create during restore")
	}
}

func TestSnapshotKeySkipsEmptyParts(t *testing.T) {
	if got := SnapshotKey("db", "", "schema", "v1"); got != "db_schema_v1" {
		t.Fatalf("unexpected snapshot key %q", got)
	}
}

type response struct {
	out []byte
	err error
}

type fakeRunner struct {
	responses map[string]response
	calls     []string
}

func (f *fakeRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	_ = ctx
	call := key(name, args...)
	f.calls = append(f.calls, call)
	if res, ok := f.responses[call]; ok {
		return res.out, res.err
	}
	return nil, nil
}

func (f *fakeRunner) sawPrefix(prefix string) bool {
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

func key(name string, args ...string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}

func jsonWrite(path string, v any) error {
	data := []byte("{\n")
	switch m := v.(type) {
	case SnapshotManifest:
		data = []byte("{\n" +
			`  "version": 1,` + "\n" +
			`  "key": "` + m.Key + `",` + "\n" +
			`  "createdAt": "0001-01-01T00:00:00Z",` + "\n" +
			`  "image": "` + m.Image + `",` + "\n" +
			`  "containerName": "` + m.ContainerName + `",` + "\n" +
			`  "volumeName": "` + m.VolumeName + `",` + "\n" +
			`  "database": "` + m.Database + `",` + "\n" +
			`  "user": "` + m.User + `",` + "\n" +
			`  "port": 55432,` + "\n" +
			`  "archivePath": "` + m.ArchivePath + `"` + "\n" +
			"}\n")
	}
	return os.WriteFile(path, data, 0o644)
}
