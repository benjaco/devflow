package database

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
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
	if db.ContainerPort != DefaultContainerPort {
		t.Fatalf("unexpected container port: %d", db.ContainerPort)
	}
	if db.SidecarImage != DefaultSidecarImage {
		t.Fatalf("unexpected sidecar image: %q", db.SidecarImage)
	}
	if !strings.Contains(db.URL, "@127.0.0.1:55432/app_wt_abc123?sslmode=disable") {
		t.Fatalf("unexpected database URL: %q", db.URL)
	}
}

func TestDesiredPreservesCustomRuntimeImagesAndContainerPort(t *testing.T) {
	mgr := New()
	db := mgr.Desired("custom", Config{
		Image:         "example/postgres:arm-ready",
		SidecarImage:  "example/tar:stable",
		HostPort:      55432,
		ContainerPort: 6432,
	})
	if db.Image != "example/postgres:arm-ready" {
		t.Fatalf("unexpected Postgres image: %q", db.Image)
	}
	if db.SidecarImage != "example/tar:stable" {
		t.Fatalf("unexpected sidecar image: %q", db.SidecarImage)
	}
	if db.ContainerPort != 6432 {
		t.Fatalf("unexpected container port: %d", db.ContainerPort)
	}
}

func TestEnsureRuntimeCreatesVolumeAndContainer(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):               {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "volume", "create", "devflow-pgdata-abc"):                {},
			key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", "postgres:16.14"): {},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         "postgres:16.14",
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

func TestEnsureRuntimePullsMissingImageBeforeColdContainerStart(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-cold"): {err: errors.New("Error: No such container: devflow-pg-cold")},
			key("docker", "image", "inspect", DefaultPostgresImage):                 {err: errors.New("Error response from daemon: No such image: " + DefaultPostgresImage)},
			key("docker", "pull", DefaultPostgresImage):                             {},
			key("docker", "volume", "inspect", "devflow-pgdata-cold"):               {err: errors.New("Error: No such volume: devflow-pgdata-cold")},
			key("docker", "volume", "create", "devflow-pgdata-cold"):                {},
			key("docker", "run", "-d", "--name", "devflow-pg-cold", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_cold", "-v", "devflow-pgdata-cold:/var/lib/postgresql/data", DefaultPostgresImage): {},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		Name:          "app_wt_cold",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         DefaultPostgresImage,
		ContainerName: "devflow-pg-cold",
		VolumeName:    "devflow-pgdata-cold",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker pull " + DefaultPostgresImage) {
		t.Fatal("expected missing image to be pulled before container start")
	}
	if !runner.calledBefore("docker pull "+DefaultPostgresImage, "docker run -d --name devflow-pg-cold") {
		t.Fatalf("expected image pull before container start, calls: %+v", runner.calls)
	}
}

func TestEnsureRuntimeReusesRunningContainerWithExpectedHostPort(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {out: []byte("true\n")},
			portInspectKey("devflow-pg-abc"):                                       {out: []byte("55432\n")},
			imageInspectKey("devflow-pg-abc"):                                      {out: []byte(DefaultPostgresImage + "\n")},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         "postgres:16.14",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if runner.sawPrefix("docker run -d --name devflow-pg-abc") {
		t.Fatal("expected existing container to be reused")
	}
}

func TestEnsureRuntimeStartsStoppedContainerWithExpectedHostPort(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {out: []byte("false\n")},
			portInspectKey("devflow-pg-abc"):                                       {out: []byte("55432\n")},
			imageInspectKey("devflow-pg-abc"):                                      {out: []byte(DefaultPostgresImage + "\n")},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):               {},
			key("docker", "start", "devflow-pg-abc"):                               {},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         "postgres:16.14",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker start devflow-pg-abc") {
		t.Fatal("expected stopped container to be started")
	}
	if runner.sawPrefix("docker run -d --name devflow-pg-abc") {
		t.Fatal("did not expect replacement container when published port matches")
	}
}

func TestEnsureRuntimeRecreatesContainerWithWrongImage(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {out: []byte("true\n")},
			portInspectKey("devflow-pg-abc"):                                       {out: []byte("55432\n")},
			imageInspectKey("devflow-pg-abc"):                                      {out: []byte("postgres:15.10\n")},
			key("docker", "rm", "-f", "devflow-pg-abc"):                            {},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):               {},
			key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", DefaultPostgresImage): {},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         DefaultPostgresImage,
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker rm -f devflow-pg-abc") {
		t.Fatal("expected container using the stale image to be removed")
	}
	if !runner.sawPrefix("docker run -d --name devflow-pg-abc") {
		t.Fatal("expected container using the configured image to be started")
	}
}

func TestEnsureRuntimeHonorsCustomContainerPort(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-custom"): {err: errors.New("Error: No such container: devflow-pg-custom")},
			key("docker", "volume", "inspect", "devflow-pgdata-custom"):               {err: errors.New("Error: No such volume: devflow-pgdata-custom")},
			key("docker", "volume", "create", "devflow-pgdata-custom"):                {},
			key("docker", "run", "-d", "--name", "devflow-pg-custom", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:6432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_custom", "-v", "devflow-pgdata-custom:/var/lib/postgresql/data", "example/postgres:custom"): {},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		Name:          "app_wt_custom",
		Port:          55432,
		ContainerPort: 6432,
		User:          "devflow",
		Password:      "secret",
		Image:         "example/postgres:custom",
		ContainerName: "devflow-pg-custom",
		VolumeName:    "devflow-pgdata-custom",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker run -d --name devflow-pg-custom") {
		t.Fatal("expected custom-port container start")
	}
}

func TestEnsureRuntimeRecreatesContainerWithWrongHostPort(t *testing.T) {
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {out: []byte("true\n")},
			portInspectKey("devflow-pg-abc"):                                       {out: []byte("55433\n")},
			key("docker", "rm", "-f", "devflow-pg-abc"):                            {},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):               {},
			key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", "postgres:16.14"): {},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         "postgres:16.14",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
	}
	if err := mgr.EnsureRuntime(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !runner.sawPrefix("docker rm -f devflow-pg-abc") {
		t.Fatal("expected stale container to be removed")
	}
	if !runner.sawPrefix("docker run -d --name devflow-pg-abc") {
		t.Fatal("expected replacement container to be started")
	}
}

func TestEnsureRuntimeTimesOutStaleContainerRemoval(t *testing.T) {
	oldTimeout := dockerControlTimeout
	dockerControlTimeout = 20 * time.Millisecond
	defer func() { dockerControlTimeout = oldTimeout }()

	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {out: []byte("true\n")},
			portInspectKey("devflow-pg-abc"):                                       {out: []byte("55433\n")},
			key("docker", "rm", "-f", "devflow-pg-abc"):                            {waitContext: true},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Port:          55432,
		User:          "devflow",
		Password:      "secret",
		Image:         "postgres:16.14",
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
	}
	err := mgr.EnsureRuntime(context.Background(), db)
	if err == nil {
		t.Fatal("expected timeout removing stale container")
	}
	if !strings.Contains(err.Error(), "docker rm -f devflow-pg-abc timed out after") {
		t.Fatalf("expected docker timeout error, got %v", err)
	}
}

func TestExecRunnerContextCancelKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell process-group cancellation test is unix-only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := (execRunner{}).CombinedOutput(ctx, "sh", "-c", "sleep 10 & wait")
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("expected process group cancellation within 1s, took %s", time.Since(start))
	}
}

func TestWaitReadyAlsoWaitsForHostPortWhenHostSet(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "exec", "devflow-pg-abc", "pg_isready", "-U", "devflow", "-d", "app_wt_abc"): {},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		Host:          "127.0.0.1",
		Port:          port,
		User:          "devflow",
		ContainerName: "devflow-pg-abc",
	}
	if err := mgr.WaitReady(context.Background(), db, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitReadyUsesCustomContainerPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "exec", "devflow-pg-custom", "pg_isready", "-U", "devflow", "-d", "app_wt_custom", "-p", "6432"): {},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		Name:          "app_wt_custom",
		Host:          "127.0.0.1",
		Port:          port,
		ContainerPort: 6432,
		User:          "devflow",
		ContainerName: "devflow-pg-custom",
	}
	if err := mgr.WaitReady(context.Background(), db, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitHostReadyReportsHostPortFailures(t *testing.T) {
	mgr := NewWithRunner(&fakeRunner{})
	db := api.DBInstance{Host: "127.0.0.1", Port: freeLocalPort(t)}
	err := mgr.WaitHostReady(context.Background(), db, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "database host port") {
		t.Fatalf("expected host readiness failure, got %v", err)
	}
}

func TestSnapshotWritesManifestAndArchiveCommand(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "stop", "-t", "10", "devflow-pg-abc"): {},
			platformInspectKey(DefaultPostgresImage):            {out: []byte("linux/arm64\n")},
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/from", "-v", filepath.Join(root, "schema_v1")+":/to", "example/tar:stable", "sh", "-c", "cd /from && tar czf /to/volume.tgz ."): {},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		Name:          "app_wt_abc",
		User:          "devflow",
		Port:          55432,
		Image:         "postgres:16.14",
		SidecarImage:  "example/tar:stable",
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
	if manifest.Version != 2 || manifest.Platform != "linux/arm64" || manifest.SidecarImage != "example/tar:stable" || manifest.ContainerPort != DefaultContainerPort {
		t.Fatalf("unexpected snapshot runtime metadata: %+v", manifest)
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
		Image:         "postgres:16.14",
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

func TestRestoreNearestPrismaSnapshotTreatsUnknownOrMismatchedPlatformAsCacheMiss(t *testing.T) {
	for _, snapshotPlatform := range []string{"", "linux/amd64"} {
		name := snapshotPlatform
		if name == "" {
			name = "legacy-unknown"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			state := &PrismaState{
				SchemaHash: "schema",
				Migrations: []PrismaMigration{{Name: "001_init", Hash: "hash"}},
				FullHash:   "full",
			}
			if _, err := SavePrismaSnapshot(root, "snapshot", state); err != nil {
				t.Fatal(err)
			}
			manifest := SnapshotManifest{
				Version:       2,
				Key:           "snapshot",
				Image:         DefaultPostgresImage,
				Platform:      snapshotPlatform,
				ContainerName: "devflow-pg-abc",
				VolumeName:    "devflow-pgdata-abc",
				Database:      "app_wt_abc",
				User:          "devflow",
				Port:          55432,
				ArchivePath:   filepath.Join(root, "snapshot", "volume.tgz"),
			}
			if err := jsonWrite(filepath.Join(root, "snapshot", "manifest.json"), manifest); err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{responses: map[string]response{
				platformInspectKey(DefaultPostgresImage): {out: []byte("linux/arm64\n")},
			}}
			mgr := NewWithRunner(runner)
			db := api.DBInstance{
				Image:         DefaultPostgresImage,
				ContainerName: "devflow-pg-abc",
				VolumeName:    "devflow-pgdata-abc",
				SnapshotRoot:  root,
			}
			result, err := mgr.RestoreNearestPrismaSnapshot(context.Background(), db, state)
			if err != nil {
				t.Fatal(err)
			}
			if result != nil {
				t.Fatalf("expected incompatible snapshot to be ignored, got %+v", result)
			}
			if runner.sawPrefix("docker rm -f") || runner.sawPrefix("docker volume rm -f") {
				t.Fatalf("expected compatibility check before destructive restore, calls: %+v", runner.calls)
			}
		})
	}
}

func TestSnapshotKeySkipsEmptyParts(t *testing.T) {
	if got := SnapshotKey("db", "", "schema", "v1"); got != "db_schema_v1" {
		t.Fatalf("unexpected snapshot key %q", got)
	}
}

func TestPostgresURLEscapesCredentialsAndIPv6(t *testing.T) {
	got := postgresURL("::1", 55432, "dev@flow", "p:a/ss", "app name")
	want := "postgres://dev%40flow:p%3Aa%2Fss@[::1]:55432/app%20name?sslmode=disable"
	if got != want {
		t.Fatalf("unexpected escaped database URL: got %q want %q", got, want)
	}
}

func TestCommandErrContainsMatchesWrappedCombinedOutput(t *testing.T) {
	err := &commandOutputError{
		err:    errors.New("exit status 1"),
		output: []byte("Error response from daemon: get devflow-pgdata-abc: no such volume\n"),
	}
	if !volumeMissing(err) {
		t.Fatal("expected wrapped combined output to match missing volume detection")
	}
}

type response struct {
	out         []byte
	err         error
	waitContext bool
}

type fakeRunner struct {
	responses map[string]response
	calls     []string
}

func (f *fakeRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	call := key(name, args...)
	f.calls = append(f.calls, call)
	if res, ok := f.responses[call]; ok {
		if res.waitContext {
			<-ctx.Done()
			return res.out, ctx.Err()
		}
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

func (f *fakeRunner) calledBefore(firstPrefix, secondPrefix string) bool {
	first := -1
	second := -1
	for index, call := range f.calls {
		if first < 0 && strings.HasPrefix(call, firstPrefix) {
			first = index
		}
		if second < 0 && strings.HasPrefix(call, secondPrefix) {
			second = index
		}
	}
	return first >= 0 && second >= 0 && first < second
}

func key(name string, args ...string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}

func portInspectKey(container string) string {
	return key("docker", "inspect", "-f", `{{range (index .NetworkSettings.Ports "5432/tcp")}}{{.HostPort}}{{"\n"}}{{end}}`, container)
}

func imageInspectKey(container string) string {
	return key("docker", "inspect", "-f", "{{.Config.Image}}", container)
}

func platformInspectKey(image string) string {
	return key("docker", "image", "inspect", "-f", "{{.Os}}/{{.Architecture}}", image)
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func jsonWrite(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
