package database

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"devflow/pkg/api"
)

func TestInspectPrismaStateAndPlanRestore(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "db", "schema.prisma"), "datasource db {}\n")
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "001_init", "migration.sql"), "create table a(id int);\n")
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "002_add_user", "migration.sql"), "create table b(id int);\n")
	mustWrite(t, filepath.Join(worktree, "db", "bootstrap.sql"), "-- bootstrap\n")

	state, err := InspectPrismaState(worktree, "db/schema.prisma", "db/migrations", []string{"db/bootstrap.sql"})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Migrations) != 2 {
		t.Fatalf("expected 2 migrations, got %d", len(state.Migrations))
	}

	root := t.TempDir()
	prefixState := *state
	prefixState.Migrations = append([]PrismaMigration(nil), state.Migrations[:1]...)
	prefixState.FullHash = hashStrings([]string{
		"schema:" + prefixState.SchemaHash,
		"base:" + prefixState.BaseFingerprint,
		prefixState.Migrations[0].Name + ":" + prefixState.Migrations[0].Hash,
	})
	if _, err := SavePrismaSnapshot(root, "prefix_001", &prefixState); err != nil {
		t.Fatal(err)
	}
	if _, err := SavePrismaSnapshot(root, "exact_002", state); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanPrismaRestore(root, state)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.ExactMatch || plan.SnapshotKey != "exact_002" {
		t.Fatalf("expected exact match plan, got %+v", plan)
	}

	mustWrite(t, filepath.Join(worktree, "db", "migrations", "003_more", "migration.sql"), "alter table b add column name text;\n")
	nextState, err := InspectPrismaState(worktree, "db/schema.prisma", "db/migrations", []string{"db/bootstrap.sql"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = PlanPrismaRestore(root, nextState)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ExactMatch {
		t.Fatalf("expected prefix plan, got exact %+v", plan)
	}
	if plan.SnapshotKey != "exact_002" || plan.PrefixLength != 2 {
		t.Fatalf("expected nearest prefix from exact_002, got %+v", plan)
	}
}

func TestRestoreNearestPrismaSnapshotUsesSelectedSnapshot(t *testing.T) {
	root := t.TempDir()
	state := &PrismaState{
		SchemaHash:      "schemahash",
		BaseFingerprint: "basehash",
		Migrations: []PrismaMigration{
			{Name: "001_init", Hash: "a"},
		},
		FullHash: "fullhash",
	}
	if _, err := SavePrismaSnapshot(root, "schema_v1", state); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "rm", "-f", "devflow-pg-abc"):               {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "rm", "-f", "devflow-pgdata-abc"): {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
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
	mustWrite(t, filepath.Join(root, "schema_v1", "volume.tgz"), "fake archive")
	if err := jsonWrite(filepath.Join(root, "schema_v1", "manifest.json"), SnapshotManifest{
		Version:       1,
		Key:           "schema_v1",
		Image:         DefaultPostgresImage,
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		Database:      "app_wt_abc",
		User:          "devflow",
		Port:          55432,
		ArchivePath:   filepath.Join(root, "schema_v1", "volume.tgz"),
	}); err != nil {
		t.Fatal(err)
	}

	result, err := mgr.RestoreNearestPrismaSnapshot(context.Background(), db, state)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Plan.SnapshotKey != "schema_v1" {
		t.Fatalf("expected restore result for schema_v1, got %+v", result)
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
