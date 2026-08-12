package database

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

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
	if err := writeVolumeMarker(ctx, db.VolumeName, "before"); err != nil {
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
	if err := writeVolumeMarker(ctx, db.VolumeName, "after"); err != nil {
		t.Fatal(err)
	}
	got, err := readVolumeMarker(ctx, db.VolumeName)
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
	got, err = readVolumeMarker(ctx, db.VolumeName)
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
	if err := writeVolumeMarker(ctx, db.VolumeName, "prisma-before"); err != nil {
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
	if err := writeVolumeMarker(ctx, db.VolumeName, "prisma-after"); err != nil {
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
	got, err := readVolumeMarker(ctx, db.VolumeName)
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

func requireDockerE2E(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Docker e2e in short mode")
	}
	if strings.TrimSpace(os.Getenv("DEVFLOW_E2E_DOCKER")) != "1" {
		t.Skip("set DEVFLOW_E2E_DOCKER=1 to enable Docker-backed integration tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not installed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "info").CombinedOutput()
	if err != nil {
		t.Skipf("docker daemon not ready: %s", strings.TrimSpace(string(out)))
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

func runPostgresContainerSQL(ctx context.Context, db api.DBInstance, query string, tuplesOnly bool) (string, error) {
	args := []string{
		"exec", db.ContainerName,
		"psql", "-X", "-v", "ON_ERROR_STOP=1",
		"-U", db.User,
		"-d", db.Name,
	}
	if tuplesOnly {
		args = append(args, "-At")
	}
	args = append(args, "-c", query)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run SQL in %s: %w: %s", db.ContainerName, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func writeVolumeMarker(ctx context.Context, volumeName, value string) error {
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"-e", "MARKER="+value,
		"-v", volumeName+":/data",
		DefaultSidecarImage,
		"sh", "-c", `printf '%s' "$MARKER" > /data/devflow-e2e-marker.txt`,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("write volume marker: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func readVolumeMarker(ctx context.Context, volumeName string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"-v", volumeName+":/data",
		DefaultSidecarImage,
		"sh", "-c", `cat /data/devflow-e2e-marker.txt`,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read volume marker: %s", strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
