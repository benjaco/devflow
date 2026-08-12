package database

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

func TestDockerEngineArchitectureMatchesHostE2E(t *testing.T) {
	requireDockerE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	architecture, err := New().dockerArchitecture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !dockerArchitectureMatchesHost(runtime.GOARCH, architecture) {
		t.Fatalf("Docker Engine architecture %q does not match native Go host architecture %q", architecture, runtime.GOARCH)
	}
}

func TestDockerRuntimeServiceLifecycleE2E(t *testing.T) {
	requireDockerE2E(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	mgr := New()
	db := e2eDBInstance(t)
	t.Cleanup(func() {
		_ = mgr.DestroyRuntime(context.Background(), db, true)
	})

	lines := make(chan string, 32)
	handle, err := mgr.StartRuntimeService(ctx, db, RuntimeServiceOptions{
		OnLine: func(stream, line string) {
			select {
			case lines <- stream + ":" + line:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.WaitReady(ctx, db, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	if handle.PID() != 0 || !handle.Alive() {
		t.Fatalf("unexpected managed-container handle state: pid=%d alive=%t", handle.PID(), handle.Alive())
	}
	sqlOutput, err := mgr.ExecSQL(ctx, db, `SELECT 'engine-api-sql';`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sqlOutput), "engine-api-sql") {
		t.Fatalf("unexpected Engine API SQL output %q", sqlOutput)
	}

	select {
	case line := <-lines:
		if !strings.HasPrefix(line, "stdout:") && !strings.HasPrefix(line, "stderr:") {
			t.Fatalf("unexpected container log stream %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a Postgres log line from the Docker Engine API")
	}

	if err := handle.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Wait(); err != nil {
		t.Fatalf("intentional managed-container stop returned %v", err)
	}
	if handle.Alive() {
		t.Fatal("managed-container handle remained alive after stop")
	}
	container, exists, err := mgr.inspectContainer(ctx, db.ContainerName)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || container.Running {
		t.Fatalf("unexpected container state after service stop: exists=%t running=%t", exists, container.Running)
	}
}

func TestDockerRuntimeSnapshotRestoreE2E(t *testing.T) {
	requireDockerE2E(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	mgr := New()
	db := e2eDBInstance(t)
	t.Cleanup(func() {
		_ = mgr.DestroyRuntime(context.Background(), db, true)
	})

	if err := mgr.EnsureRuntime(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := mgr.WaitReady(ctx, db, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeMarker(ctx, db, "before"); err != nil {
		t.Fatal(err)
	}

	manifest, err := mgr.Snapshot(ctx, db, "snap_v1")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Key != "snap_v1" {
		t.Fatalf("unexpected snapshot key %q", manifest.Key)
	}
	if _, err := os.Stat(manifest.ArchivePath); err != nil {
		t.Fatalf("expected snapshot archive to exist: %v", err)
	}

	if err := mgr.EnsureRuntime(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := mgr.WaitReady(ctx, db, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeMarker(ctx, db, "after"); err != nil {
		t.Fatal(err)
	}
	got, err := readVolumeMarker(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if got != "after" {
		t.Fatalf("expected mutated marker before restore, got %q", got)
	}

	if _, err := mgr.RestoreSnapshot(ctx, db, "snap_v1"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.EnsureRuntime(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := mgr.WaitReady(ctx, db, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	got, err = readVolumeMarker(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if got != "before" {
		t.Fatalf("expected restored marker %q, got %q", "before", got)
	}
}

func TestDockerPrismaSnapshotRestoreNearestE2E(t *testing.T) {
	requireDockerE2E(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	mgr := New()
	db := e2eDBInstance(t)
	t.Cleanup(func() {
		_ = mgr.DestroyRuntime(context.Background(), db, true)
	})

	state := &PrismaState{
		SchemaHash:      "schemahash",
		BaseFingerprint: "basehash",
		Migrations: []PrismaMigration{
			{Name: "001_init", Hash: "h1"},
		},
		FullHash: "fullhash",
	}

	if err := mgr.EnsureRuntime(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := mgr.WaitReady(ctx, db, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeMarker(ctx, db, "prisma-before"); err != nil {
		t.Fatal(err)
	}

	result, err := mgr.SnapshotPrisma(ctx, db, "prisma_v1", state)
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan.SnapshotKey != "prisma_v1" {
		t.Fatalf("unexpected snapshot plan %+v", result.Plan)
	}

	if err := mgr.EnsureRuntime(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := mgr.WaitReady(ctx, db, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := writeVolumeMarker(ctx, db, "prisma-after"); err != nil {
		t.Fatal(err)
	}

	restore, err := mgr.RestoreNearestPrismaSnapshot(ctx, db, state)
	if err != nil {
		t.Fatal(err)
	}
	if restore == nil || !restore.Plan.ExactMatch || restore.Plan.SnapshotKey != "prisma_v1" {
		t.Fatalf("unexpected restore result %+v", restore)
	}

	if err := mgr.EnsureRuntime(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := mgr.WaitReady(ctx, db, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	got, err := readVolumeMarker(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if got != "prisma-before" {
		t.Fatalf("expected restored Prisma marker %q, got %q", "prisma-before", got)
	}
}

func TestDockerPostgresDumpSourcePolicyClonesSchemaAndDataFromNonDefaultPortE2E(t *testing.T) {
	requireDockerE2E(t)
	requirePostgresClientsE2E(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	mgr := New()
	sourcePort := freePortExcluding(t, DefaultContainerPort)
	targetPort := freePortExcluding(t, DefaultContainerPort, sourcePort)
	source := e2eDBInstanceOnPort(t, "source", "devflow_source", sourcePort)
	target := e2eDBInstanceOnPort(t, "target", "devflow_target", targetPort)
	t.Cleanup(func() {
		_ = mgr.DestroyRuntime(context.Background(), source, true)
		_ = mgr.DestroyRuntime(context.Background(), target, true)
	})

	for _, database := range []struct {
		role string
		db   api.DBInstance
	}{
		{role: "source", db: source},
		{role: "target", db: target},
	} {
		if err := mgr.EnsureRuntime(ctx, database.db); err != nil {
			t.Fatalf("start %s database: %v", database.role, err)
		}
		if err := mgr.WaitReady(ctx, database.db, 45*time.Second); err != nil {
			t.Fatalf("wait for %s database: %v", database.role, err)
		}
	}

	seedSQL := `
CREATE SCHEMA inventory;
CREATE TABLE inventory.widgets (
    id integer PRIMARY KEY,
    name text NOT NULL,
    quantity integer NOT NULL,
    details jsonb NOT NULL
);
INSERT INTO inventory.widgets (id, name, quantity, details) VALUES
    (1, 'socket wrench', 4, '{"origin":"remote"}'),
    (2, 'torque key', 7, '{"origin":"remote"}');`
	if _, err := runPostgresContainerSQL(ctx, source, seedSQL, false); err != nil {
		t.Fatal(err)
	}

	policy := PostgresDumpSourcePolicy{
		PolicyName: "clone-e2e-source",
		RemoteURL:  source.URL,
	}
	worktree := t.TempDir()
	cloneLog := filepath.Join(worktree, "postgres-clone.log")
	if err := policy.PrepareBase(ctx, target, PrepareOptions{
		Worktree: worktree,
		LogPath:  cloneLog,
	}); err != nil {
		logOutput, _ := os.ReadFile(cloneLog)
		t.Fatalf("clone source database on port %d into target on port %d: %v\nclone log:\n%s", source.Port, target.Port, err, logOutput)
	}

	query := `SELECT id::text || '|' || name || '|' || quantity::text || '|' || (details->>'origin')
FROM inventory.widgets
ORDER BY id;`
	got, err := runPostgresContainerSQL(ctx, target, query, true)
	if err != nil {
		t.Fatal(err)
	}
	want := "1|socket wrench|4|remote\n2|torque key|7|remote"
	if got != want {
		t.Fatalf("unexpected cloned rows:\n%s\nwant:\n%s", got, want)
	}
}

func TestDockerPostGISRuntimeSpatialQueryE2E(t *testing.T) {
	requireDockerE2E(t)

	for _, postgresVersion := range []int{16, 17, 18} {
		t.Run(strconv.Itoa(postgresVersion), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
			defer cancel()

			mgr := New()
			db := e2ePostGISDBInstance(t, postgresVersion)
			t.Cleanup(func() {
				_ = mgr.DestroyRuntime(context.Background(), db, true)
			})

			if err := mgr.EnsureRuntime(ctx, db); err != nil {
				t.Fatal(err)
			}
			dockerArchitecture := assertPostGISRuntimeImageMatchesDockerArchitecture(t, ctx, mgr, db)
			if err := mgr.WaitReady(ctx, db, 90*time.Second); err != nil {
				t.Fatal(err)
			}

			query := `SELECT
    extversion || '|' ||
    ST_AsText(ST_SetSRID(ST_MakePoint(12.5683, 55.6761), 4326)) || '|' ||
    round(ST_Distance(
        ST_SetSRID(ST_MakePoint(12.5683, 55.6761), 4326)::geography,
        ST_SetSRID(ST_MakePoint(12.5683, 55.6861), 4326)::geography
    ))::text
FROM pg_extension
WHERE extname = 'postgis';`
			got, err := runPostgresContainerSQL(ctx, db, query, true)
			if err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(got, "|")
			runtime, err := postGISRuntimeForVersion(postgresVersion)
			if err != nil {
				t.Fatal(err)
			}
			postGISVersionPrefix := "3."
			if dockerArchitecture == "amd64" || dockerArchitecture == "x86_64" {
				postGISVersionPrefix = runtime.upstreamPostGISVersion + "."
			}
			if len(parts) != 3 || !strings.HasPrefix(parts[0], postGISVersionPrefix) || parts[1] != "POINT(12.5683 55.6761)" {
				t.Fatalf("unexpected PostGIS result %q", got)
			}
			distance, err := strconv.Atoi(parts[2])
			if err != nil {
				t.Fatalf("parse PostGIS distance from %q: %v", got, err)
			}
			if distance < 1100 || distance > 1120 {
				t.Fatalf("unexpected PostGIS geography distance %d meters", distance)
			}

			serverMajor, err := runPostgresContainerSQL(ctx, db, `SELECT current_setting('server_version_num')::int / 10000;`, true)
			if err != nil {
				t.Fatal(err)
			}
			if serverMajor != strconv.Itoa(postgresVersion) {
				t.Fatalf("runtime reported PostgreSQL major %q, want %d", serverMajor, postgresVersion)
			}

			if _, err := runPostgresContainerSQL(ctx, db, `CREATE TABLE devflow_postgis_persistence_probe (
    id integer PRIMARY KEY,
    location geometry(Point, 4326) NOT NULL
);
INSERT INTO devflow_postgis_persistence_probe (id, location)
VALUES (1, ST_SetSRID(ST_MakePoint(12.5683, 55.6761), 4326));`, false); err != nil {
				t.Fatal(err)
			}
			if err := mgr.DestroyRuntime(ctx, db, false); err != nil {
				t.Fatal(err)
			}
			if err := mgr.EnsureRuntime(ctx, db); err != nil {
				t.Fatal(err)
			}
			if err := mgr.WaitReady(ctx, db, 90*time.Second); err != nil {
				t.Fatal(err)
			}
			persisted, err := runPostgresContainerSQL(ctx, db, `SELECT id::text || '|' || ST_AsText(location)
FROM devflow_postgis_persistence_probe;`, true)
			if err != nil {
				t.Fatal(err)
			}
			if persisted != "1|POINT(12.5683 55.6761)" {
				t.Fatalf("unexpected persisted PostGIS row %q", persisted)
			}
		})
	}
}

func assertPostGISRuntimeImageMatchesDockerArchitecture(t *testing.T, ctx context.Context, mgr *Manager, db api.DBInstance) string {
	t.Helper()
	architecture, err := mgr.dockerArchitecture(ctx)
	if err != nil {
		t.Fatalf("inspect Docker architecture through Engine API: %v", err)
	}
	container, exists, err := mgr.inspectContainer(ctx, db.ContainerName)
	if err != nil {
		t.Fatalf("inspect PostGIS runtime container through Engine API: %v", err)
	}
	if !exists {
		t.Fatalf("PostGIS runtime container %s disappeared", db.ContainerName)
	}
	want, err := postGISImageForArchitecture(db.PostgresVersion, architecture)
	if err != nil {
		t.Fatal(err)
	}
	if got := container.Image; got != want {
		t.Fatalf("unexpected PostGIS image %q for Docker architecture %q, want %q", got, architecture, want)
	}
	return architecture
}

func requireDockerE2E(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Docker e2e in short mode")
	}
	if strings.TrimSpace(os.Getenv("DEVFLOW_E2E_DOCKER")) != "1" {
		t.Skip("set DEVFLOW_E2E_DOCKER=1 to enable Docker-backed integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := New().pingDocker(ctx); err != nil {
		t.Skipf("Docker Engine not ready through the Go API: %v", err)
	}
}

func requirePostgresClientsE2E(t *testing.T) {
	t.Helper()
	for _, name := range []string{"pg_dump", "psql"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is required for the remote Postgres clone e2e test: %v", name, err)
		}
	}
}

func e2eDBInstance(t *testing.T) api.DBInstance {
	t.Helper()
	return e2eDBInstanceOnPort(t, "", "devflow_e2e", freePort(t))
}

func e2eDBInstanceOnPort(t *testing.T, role, database string, hostPort int) api.DBInstance {
	t.Helper()
	mgr := New()
	instanceID := fmt.Sprintf("e2e%s%x", role, time.Now().UnixNano())
	return mgr.Desired(instanceID, Config{
		HostPort:     hostPort,
		Database:     database,
		User:         "devflow",
		Password:     "devflow",
		SnapshotRoot: t.TempDir(),
	})
}

func e2ePostGISDBInstance(t *testing.T, postgresVersion int) api.DBInstance {
	t.Helper()
	mgr := New()
	instanceID := fmt.Sprintf("e2epostgis%x", time.Now().UnixNano())
	return mgr.Desired(instanceID, Config{
		Flavor:          FlavorPostGIS,
		PostgresVersion: postgresVersion,
		HostPort:        freePortExcluding(t, DefaultContainerPort),
		Database:        "devflow_postgis_e2e",
		User:            "devflow",
		Password:        "devflow",
		SnapshotRoot:    t.TempDir(),
	})
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func freePortExcluding(t *testing.T, excluded ...int) int {
	t.Helper()
	for attempts := 0; attempts < 10; attempts++ {
		port := freePort(t)
		matchesExcluded := false
		for _, value := range excluded {
			if port == value {
				matchesExcluded = true
				break
			}
		}
		if !matchesExcluded {
			return port
		}
	}
	t.Fatal("could not allocate a distinct non-default Postgres host port")
	return 0
}

func dockerArchitectureMatchesHost(hostArchitecture, dockerArchitecture string) bool {
	hostArchitecture = strings.ToLower(strings.TrimSpace(hostArchitecture))
	dockerArchitecture = strings.ToLower(strings.TrimSpace(dockerArchitecture))
	switch hostArchitecture {
	case "amd64":
		return dockerArchitecture == "amd64" || dockerArchitecture == "x86_64"
	case "arm64":
		return dockerArchitecture == "arm64" || dockerArchitecture == "aarch64"
	default:
		return hostArchitecture == dockerArchitecture
	}
}

func runPostgresContainerSQL(ctx context.Context, db api.DBInstance, query string, tuplesOnly bool) (string, error) {
	args := []string{
		"psql", "-X", "-v", "ON_ERROR_STOP=1",
		"-U", db.User,
		"-d", db.Name,
	}
	if tuplesOnly {
		args = append(args, "-At")
	}
	args = append(args, "-c", query)
	out, err := New().execContainer(ctx, dockerDataTimeout, db.ContainerName, args)
	if err != nil {
		return "", fmt.Errorf("run SQL in %s through Docker Engine API: %w: %s", db.ContainerName, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func writeVolumeMarker(ctx context.Context, db api.DBInstance, value string) error {
	markerPath := postgresVolumeMount(db) + "/devflow-e2e-marker.txt"
	out, err := New().execContainer(ctx, dockerControlTimeout, db.ContainerName, []string{
		"sh", "-c", `printf '%s' "$1" > "$2"`, "devflow-marker", value, markerPath,
	})
	if err != nil {
		return fmt.Errorf("write volume marker through Docker Engine API: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func readVolumeMarker(ctx context.Context, db api.DBInstance) (string, error) {
	markerPath := postgresVolumeMount(db) + "/devflow-e2e-marker.txt"
	out, err := New().execContainer(ctx, dockerControlTimeout, db.ContainerName, []string{"cat", markerPath})
	if err != nil {
		return "", fmt.Errorf("read volume marker through Docker Engine API: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
