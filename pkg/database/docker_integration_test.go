package database

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
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

func e2eDBInstance(t *testing.T) api.DBInstance {
	t.Helper()
	mgr := New()
	instanceID := fmt.Sprintf("e2e%x", time.Now().UnixNano())
	return mgr.Desired(instanceID, Config{
		HostPort:     freePort(t),
		Database:     "devflow_e2e",
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
