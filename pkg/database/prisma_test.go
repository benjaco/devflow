package database

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/process"
)

func TestInspectPrismaStateAndPlanRestore(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "db", "schema.prisma"), "datasource db {}\n")
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "001_init", "migration.sql"), "create table a(id int);\n")
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "002_add_user", "migration.sql"), "create table b(id int);\n")
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "migration_lock.toml"), "provider = \"postgresql\"\n")
	mustWrite(t, filepath.Join(worktree, "db", "bootstrap.sql"), "-- bootstrap\n")

	state, err := InspectPrismaState(worktree, "db/schema.prisma", "db/migrations", []string{"db/bootstrap.sql"})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Migrations) != 2 {
		t.Fatalf("expected 2 directory migrations, got %+v", state.Migrations)
	}
	for _, migration := range state.Migrations {
		if migration.Name == "migration_lock.toml" {
			t.Fatalf("expected migration_lock.toml to be ignored, got %+v", state.Migrations)
		}
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

func TestPrismaMigrationLockChurnDoesNotChangeState(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key);\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "migration_lock.toml"), "provider = \"postgresql\"\n")

	before, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "migration_lock.toml"), "provider = \"postgresql\"\n# rewritten by prisma\n")
	after, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}

	if before.FullHash != after.FullHash {
		t.Fatalf("expected migration_lock.toml churn not to affect Prisma state hash, before %s after %s", before.FullHash, after.FullHash)
	}
	if PrismaSnapshotKey(before) != PrismaSnapshotKey(after) {
		t.Fatalf("expected migration_lock.toml churn not to affect snapshot key, before %s after %s", PrismaSnapshotKey(before), PrismaSnapshotKey(after))
	}
	if len(after.Migrations) != 1 || after.Migrations[0].Name != "001_init" {
		t.Fatalf("expected only directory migrations, got %+v", after.Migrations)
	}
}

func TestInspectPrismaDevelopmentStatusFlagsSchemaWithoutMigration(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key);\n")
	oldState, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	if _, err := SavePrismaSnapshot(snapshotRoot, PrismaSnapshotKey(oldState), oldState); err != nil {
		t.Fatal(err)
	}

	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id name String }\n")
	status, err := InspectPrismaDevelopmentStatus(worktree, "prisma/schema.prisma", "prisma/migrations", nil, snapshotRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !status.NeedsNewMigration || status.Reason != "schema_changed" {
		t.Fatalf("expected schema-changed migration status, got %+v", status)
	}
}

func TestInspectPrismaDevelopmentStatusFlagsModelSchemaWithoutMigrations(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id }\n")

	status, err := InspectPrismaDevelopmentStatus(worktree, "prisma/schema.prisma", "prisma/migrations", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !status.NeedsNewMigration || status.Reason != "no_migrations" {
		t.Fatalf("expected no-migrations status, got %+v", status)
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

func TestPreparePrismaBaseUsesSnapshotWithoutApplyingSource(t *testing.T) {
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
	called := false
	result, err := mgr.PreparePrismaBase(context.Background(), db, state, SourcePolicyFunc{
		PolicyName: "clone-dev",
		Fn: func(ctx context.Context, db api.DBInstance, opts PrepareOptions) error {
			called = true
			return nil
		},
	}, PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("expected source policy not to run when a snapshot is restored")
	}
	if result == nil || result.Restored == nil || result.Restored.Plan.SnapshotKey != "schema_v1" {
		t.Fatalf("expected restored snapshot result, got %+v", result)
	}
}

func TestPreparePrismaBaseRecreatesAndAppliesSourceOnMiss(t *testing.T) {
	root := t.TempDir()
	state := &PrismaState{
		SchemaHash:      "schemahash",
		BaseFingerprint: "basehash",
		Migrations:      []PrismaMigration{{Name: "001_init", Hash: "a"}},
		FullHash:        "fullhash",
	}
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "rm", "-f", "devflow-pg-abc"):                            {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "rm", "-f", "devflow-pgdata-abc"):              {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):               {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "volume", "create", "devflow-pgdata-abc"):                {},
			key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", "postgres:16.3"): {},
			key("docker", "exec", "devflow-pg-abc", "pg_isready", "-U", "devflow", "-d", "app_wt_abc"): {},
			key("docker", "stop", "-t", "10", "devflow-pg-abc"):                                        {},
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
		SnapshotRoot:  root,
		URL:           "postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
	}
	called := false
	var gotEnv map[string]string
	result, err := mgr.PreparePrismaBase(context.Background(), db, state, SourcePolicyFunc{
		PolicyName: "clone-dev",
		Fn: func(ctx context.Context, db api.DBInstance, opts PrepareOptions) error {
			called = true
			gotEnv = opts.Env
			return nil
		},
	}, PrepareOptions{
		Worktree: root,
		Env: map[string]string{
			"REMOTE_URL": "postgres://remote",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("expected source policy to run on snapshot miss")
	}
	if result == nil || !result.Recreated || !result.SourceApplied || result.SourcePolicy != "clone-dev" {
		t.Fatalf("unexpected prepare result: %+v", result)
	}
	if gotEnv["REMOTE_URL"] != "postgres://remote" {
		t.Fatalf("expected prepare options env to be forwarded, got %+v", gotEnv)
	}
	if !runner.sawPrefix("docker run -d --name devflow-pg-abc") {
		t.Fatal("expected runtime to be created before applying source policy")
	}
}

func TestPreparePrismaBaseRecreatesEmptyVolumeWithoutSourcePolicy(t *testing.T) {
	root := t.TempDir()
	state := &PrismaState{
		SchemaHash:      "schemahash",
		BaseFingerprint: "basehash",
		Migrations:      []PrismaMigration{{Name: "001_init", Hash: "a"}},
		FullHash:        "fullhash",
	}
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "rm", "-f", "devflow-pg-abc"):               {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "rm", "-f", "devflow-pgdata-abc"): {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
		},
	}
	mgr := NewWithRunner(runner)
	db := api.DBInstance{
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		SnapshotRoot:  root,
	}
	result, err := mgr.PreparePrismaBase(context.Background(), db, state, nil, PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.Recreated || result.SourceApplied || result.Restored != nil {
		t.Fatalf("unexpected prepare result: %+v", result)
	}
	if runner.sawPrefix("docker run -d --name devflow-pg-abc") {
		t.Fatal("did not expect runtime creation without a source policy")
	}
}

func TestEnsurePrismaDevDatabaseAllowsModelFreeSchemaWithoutMigrations(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), `
datasource db {
  provider = "postgresql"
  url      = env("DATABASE_URL")
}

generator client {
  provider = "prisma-client-js"
}
`)
	state, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Migrations) != 0 {
		t.Fatalf("expected no Prisma migrations, got %+v", state.Migrations)
	}
	snapshotRoot := t.TempDir()
	finalKey := PrismaSnapshotKey(state)
	runner := &fakeRunner{responses: prismaRuntimeResponses(snapshotRoot, finalKey)}
	mgr := NewWithRunner(runner)

	result, err := mgr.EnsurePrismaDevDatabase(context.Background(), PrismaDevDatabaseOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			t.Fatalf("did not expect model-free Prisma schema to apply migration %s", migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Applied || result.SnapshotKey != finalKey || !result.Plan.ExactMatch {
		t.Fatalf("unexpected model-free Prisma result: %+v", result)
	}
	if result.Base == nil || !result.Base.Recreated {
		t.Fatalf("expected empty runtime to be prepared for model-free schema, got %+v", result.Base)
	}
	if _, err := os.Stat(filepath.Join(snapshotRoot, finalKey, "prisma.json")); err != nil {
		t.Fatalf("expected model-free Prisma snapshot metadata: %v", err)
	}
}

func TestEnsurePrismaDevDatabaseRestoresPrefixDeploysAndSnapshots(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table a(id int);\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "002_users", "migration.sql"), "create table users(id int);\n")
	state, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	prefixState := *state
	prefixState.Migrations = append([]PrismaMigration(nil), state.Migrations[:1]...)
	prefixState.FullHash = hashStrings([]string{
		"schema:" + prefixState.SchemaHash,
		"base:" + prefixState.BaseFingerprint,
		prefixState.Migrations[0].Name + ":" + prefixState.Migrations[0].Hash,
	})
	snapshotRoot := t.TempDir()
	if _, err := SavePrismaSnapshot(snapshotRoot, "prefix_001", &prefixState); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(snapshotRoot, "prefix_001", "volume.tgz"), "fake")
	if err := jsonWrite(filepath.Join(snapshotRoot, "prefix_001", "manifest.json"), SnapshotManifest{
		Version:       1,
		Key:           "prefix_001",
		Image:         DefaultPostgresImage,
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		Database:      "app_wt_abc",
		User:          "devflow",
		Port:          55432,
		ArchivePath:   filepath.Join(snapshotRoot, "prefix_001", "volume.tgz"),
	}); err != nil {
		t.Fatal(err)
	}
	finalKey := PrismaSnapshotKey(state)
	applyMarker := filepath.Join(worktree, "prisma-deployed.txt")
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "rm", "-f", "devflow-pg-abc"):               {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "rm", "-f", "devflow-pgdata-abc"): {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):  {},
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/to", "-v", filepath.Join(snapshotRoot, "prefix_001")+":/from", DefaultSidecarImage, "sh", "-c", "cd /to && tar xzf /from/volume.tgz"): {},
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {},
			portInspectKey("devflow-pg-abc"):                                       {},
			key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", "postgres:16.3"): {},
			key("docker", "exec", "devflow-pg-abc", "pg_isready", "-U", "devflow", "-d", "app_wt_abc"): {},
			key("docker", "stop", "-t", "10", "devflow-pg-abc"):                                        {},
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/from", "-v", filepath.Join(snapshotRoot, finalKey)+":/to", DefaultSidecarImage, "sh", "-c", "cd /from && tar czf /to/volume.tgz ."): {},
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
		SnapshotRoot:  snapshotRoot,
		URL:           "postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
	}
	result, err := mgr.EnsurePrismaDevDatabase(context.Background(), PrismaDevDatabaseOptions{
		Worktree:      worktree,
		DB:            db,
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		Migrate: process.CommandSpec{
			Name: "sh",
			Args: []string{"-c", "printf deploy > " + strconv.Quote(applyMarker)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.Applied || result.SnapshotKey != finalKey || !result.Plan.ExactMatch {
		t.Fatalf("unexpected prisma workflow result: %+v", result)
	}
	data, err := os.ReadFile(applyMarker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "deploy" {
		t.Fatalf("expected prisma migrate command to run, got %q", string(data))
	}
	if _, err := os.Stat(filepath.Join(snapshotRoot, finalKey, "prisma.json")); err != nil {
		t.Fatalf("expected final prisma snapshot metadata: %v", err)
	}
}

func TestEnsurePrismaDevDatabaseSnapshotsEachPrismaMigrationPrefix(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table a(id int);\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "002_users", "migration.sql"), "create table users(id int);\n")
	state, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	firstKey := PrismaSnapshotKey(prismaStatePrefix(state, 1))
	finalKey := PrismaSnapshotKey(prismaStatePrefix(state, 2))
	snapshotRoot := t.TempDir()
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "rm", "-f", "devflow-pg-abc"):                            {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "rm", "-f", "devflow-pgdata-abc"):              {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):               {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "volume", "create", "devflow-pgdata-abc"):                {},
			key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", "postgres:16.3"): {},
			key("docker", "exec", "devflow-pg-abc", "pg_isready", "-U", "devflow", "-d", "app_wt_abc"): {},
			key("docker", "stop", "-t", "10", "devflow-pg-abc"):                                        {},
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/from", "-v", filepath.Join(snapshotRoot, firstKey)+":/to", DefaultSidecarImage, "sh", "-c", "cd /from && tar czf /to/volume.tgz ."): {},
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/from", "-v", filepath.Join(snapshotRoot, finalKey)+":/to", DefaultSidecarImage, "sh", "-c", "cd /from && tar czf /to/volume.tgz ."): {},
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
		SnapshotRoot:  snapshotRoot,
		URL:           "postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
	}
	applied := make([]string, 0, 2)
	result, err := mgr.EnsurePrismaDevDatabase(context.Background(), PrismaDevDatabaseOptions{
		Worktree:      worktree,
		DB:            db,
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			applied = append(applied, migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(applied, ",") != "001_init,002_users" {
		t.Fatalf("unexpected applied prisma migrations: %v", applied)
	}
	if result == nil || result.SnapshotKey != finalKey || !result.Plan.ExactMatch {
		t.Fatalf("unexpected prisma per-migration result: %+v", result)
	}
	for _, key := range []string{firstKey, finalKey} {
		if _, err := os.Stat(filepath.Join(snapshotRoot, key, "prisma.json")); err != nil {
			t.Fatalf("expected prisma prefix snapshot %s: %v", key, err)
		}
	}
}

func TestEnsurePrismaDevDatabaseReusesExactSnapshotWithoutApplying(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table a(id int);\n")
	state, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	finalKey := PrismaSnapshotKey(state)
	if _, err := SavePrismaSnapshot(snapshotRoot, finalKey, state); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(snapshotRoot, finalKey, "volume.tgz"), "fake")
	if err := jsonWrite(filepath.Join(snapshotRoot, finalKey, "manifest.json"), SnapshotManifest{
		Version:       1,
		Key:           finalKey,
		Image:         DefaultPostgresImage,
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		Database:      "app_wt_abc",
		User:          "devflow",
		Port:          55432,
		ArchivePath:   filepath.Join(snapshotRoot, finalKey, "volume.tgz"),
	}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "rm", "-f", "devflow-pg-abc"):               {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "rm", "-f", "devflow-pgdata-abc"): {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):  {},
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/to", "-v", filepath.Join(snapshotRoot, finalKey)+":/from", DefaultSidecarImage, "sh", "-c", "cd /to && tar xzf /from/volume.tgz"): {},
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {},
			portInspectKey("devflow-pg-abc"):                                       {},
			key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", "postgres:16.3"): {},
			key("docker", "exec", "devflow-pg-abc", "pg_isready", "-U", "devflow", "-d", "app_wt_abc"): {},
		},
	}
	mgr := NewWithRunner(runner)
	result, err := mgr.EnsurePrismaDevDatabase(context.Background(), PrismaDevDatabaseOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			t.Fatalf("did not expect exact Prisma snapshot to apply migration %s", migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Applied || !result.Plan.ExactMatch || result.Plan.SnapshotKey != finalKey {
		t.Fatalf("unexpected exact Prisma result: %+v", result)
	}
	if runner.sawPrefix("docker stop -t 10 devflow-pg-abc") {
		t.Fatal("did not expect final snapshot when exact Prisma snapshot was restored")
	}
}

func TestEnsurePrismaDevDatabaseNewMigrationRestoresExistingPrefix(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key);\n")
	prefixState, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	prefixKey := PrismaSnapshotKey(prefixState)
	writePrismaSnapshotFixture(t, snapshotRoot, prefixKey, prefixState)

	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id }\nmodel Post { id Int @id userId Int }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "002_posts", "migration.sql"), "create table posts(id int primary key, user_id int not null);\n")
	current, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	finalKey := PrismaSnapshotKey(current)
	responses := prismaRuntimeResponses(snapshotRoot, finalKey)
	addPrismaRestoreResponse(responses, snapshotRoot, prefixKey)
	runner := &fakeRunner{responses: responses}
	mgr := NewWithRunner(runner)
	applied := make([]string, 0, 1)

	result, err := mgr.EnsurePrismaDevDatabase(context.Background(), PrismaDevDatabaseOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			applied = append(applied, migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(applied, ",") != "002_posts" {
		t.Fatalf("expected only new Prisma migration to apply, got %v", applied)
	}
	if result == nil || !result.Applied || result.SnapshotKey != finalKey || !result.Plan.ExactMatch {
		t.Fatalf("unexpected new Prisma migration result: %+v", result)
	}
	if result.Base == nil || result.Base.Restored == nil || result.Base.Restored.Plan.PrefixLength != 1 {
		t.Fatalf("expected one-migration prefix restore, got %+v", result.Base)
	}
}

func TestEnsurePrismaDevDatabaseMultipleNewMigrationsApplyTailInOrder(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key);\n")
	prefixState, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	prefixKey := PrismaSnapshotKey(prefixState)
	writePrismaSnapshotFixture(t, snapshotRoot, prefixKey, prefixState)

	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id name String? }\nmodel Post { id Int @id userId Int }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "002_user_name", "migration.sql"), "alter table users add column name text;\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "003_posts", "migration.sql"), "create table posts(id int primary key, user_id int not null);\n")
	current, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	secondKey := PrismaSnapshotKey(prismaStatePrefix(current, 2))
	finalKey := PrismaSnapshotKey(current)
	responses := prismaRuntimeResponses(snapshotRoot, secondKey, finalKey)
	addPrismaRestoreResponse(responses, snapshotRoot, prefixKey)
	runner := &fakeRunner{responses: responses}
	mgr := NewWithRunner(runner)
	applied := make([]string, 0, 2)

	result, err := mgr.EnsurePrismaDevDatabase(context.Background(), PrismaDevDatabaseOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			applied = append(applied, migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(applied, ",") != "002_user_name,003_posts" {
		t.Fatalf("expected new Prisma migrations to apply in order, got %v", applied)
	}
	if result == nil || !result.Applied || result.SnapshotKey != finalKey || !result.Plan.ExactMatch {
		t.Fatalf("unexpected multiple Prisma migration result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(snapshotRoot, secondKey, "prisma.json")); err != nil {
		t.Fatalf("expected intermediate Prisma prefix snapshot: %v", err)
	}
}

func TestEnsurePrismaDevDatabaseChangedLatestMigrationAppliesOnlyTail(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table a(id int);\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "002_users", "migration.sql"), "create table users(id int);\n")
	original, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	prefixState := prismaStatePrefix(original, 1)
	snapshotRoot := t.TempDir()
	prefixKey := PrismaSnapshotKey(prefixState)
	if _, err := SavePrismaSnapshot(snapshotRoot, prefixKey, prefixState); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(snapshotRoot, prefixKey, "volume.tgz"), "fake")
	if err := jsonWrite(filepath.Join(snapshotRoot, prefixKey, "manifest.json"), SnapshotManifest{
		Version:       1,
		Key:           prefixKey,
		Image:         DefaultPostgresImage,
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		Database:      "app_wt_abc",
		User:          "devflow",
		Port:          55432,
		ArchivePath:   filepath.Join(snapshotRoot, prefixKey, "volume.tgz"),
	}); err != nil {
		t.Fatal(err)
	}

	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "002_users", "migration.sql"), "create table users(id int, name text);\n")
	current, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	finalKey := PrismaSnapshotKey(current)
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "rm", "-f", "devflow-pg-abc"):               {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "rm", "-f", "devflow-pgdata-abc"): {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):  {},
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/to", "-v", filepath.Join(snapshotRoot, prefixKey)+":/from", DefaultSidecarImage, "sh", "-c", "cd /to && tar xzf /from/volume.tgz"): {},
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {},
			portInspectKey("devflow-pg-abc"):                                       {},
			key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", "postgres:16.3"): {},
			key("docker", "exec", "devflow-pg-abc", "pg_isready", "-U", "devflow", "-d", "app_wt_abc"): {},
			key("docker", "stop", "-t", "10", "devflow-pg-abc"):                                        {},
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/from", "-v", filepath.Join(snapshotRoot, finalKey)+":/to", DefaultSidecarImage, "sh", "-c", "cd /from && tar czf /to/volume.tgz ."): {},
		},
	}
	mgr := NewWithRunner(runner)
	applied := make([]string, 0, 1)
	result, err := mgr.EnsurePrismaDevDatabase(context.Background(), PrismaDevDatabaseOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			applied = append(applied, migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(applied, ",") != "002_users" {
		t.Fatalf("expected only changed Prisma tail migration to apply, got %v", applied)
	}
	if result == nil || result.SnapshotKey != finalKey || !result.Plan.ExactMatch {
		t.Fatalf("unexpected changed Prisma tail result: %+v", result)
	}
}

func TestEnsurePrismaDevDatabaseDeletedLatestMigrationRestoresOlderExactSnapshot(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key);\n")
	firstState, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "002_posts", "migration.sql"), "create table posts(id int primary key);\n")
	twoMigrationState, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	firstKey := PrismaSnapshotKey(firstState)
	finalOldKey := PrismaSnapshotKey(twoMigrationState)
	writePrismaSnapshotFixture(t, snapshotRoot, firstKey, firstState)
	writePrismaSnapshotFixture(t, snapshotRoot, finalOldKey, twoMigrationState)
	if err := os.RemoveAll(filepath.Join(worktree, "prisma", "migrations", "002_posts")); err != nil {
		t.Fatal(err)
	}
	current, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	if PrismaSnapshotKey(current) != firstKey {
		t.Fatalf("test setup expected deleted latest migration to return to first snapshot key, got %s want %s", PrismaSnapshotKey(current), firstKey)
	}
	responses := prismaRuntimeResponses(snapshotRoot)
	addPrismaRestoreResponse(responses, snapshotRoot, firstKey)
	runner := &fakeRunner{responses: responses}
	mgr := NewWithRunner(runner)

	result, err := mgr.EnsurePrismaDevDatabase(context.Background(), PrismaDevDatabaseOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			t.Fatalf("did not expect deleted-latest Prisma state to apply migration %s", migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Applied || !result.Plan.ExactMatch || result.Plan.SnapshotKey != firstKey {
		t.Fatalf("unexpected deleted latest migration result: %+v", result)
	}
	if runner.sawPrefix("docker stop -t 10 devflow-pg-abc") {
		t.Fatal("did not expect a new snapshot when exact older Prisma snapshot was restored")
	}
}

func TestEnsurePrismaDevDatabaseChangedOlderMigrationRebuildsFromSource(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key);\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "002_users_name", "migration.sql"), "alter table users add column name text;\n")
	original, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	writePrismaSnapshotFixture(t, snapshotRoot, PrismaSnapshotKey(prismaStatePrefix(original, 1)), prismaStatePrefix(original, 1))
	writePrismaSnapshotFixture(t, snapshotRoot, PrismaSnapshotKey(original), original)

	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key, created_at timestamp);\n")
	current, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	firstKey := PrismaSnapshotKey(prismaStatePrefix(current, 1))
	finalKey := PrismaSnapshotKey(current)
	runner := &fakeRunner{responses: prismaRuntimeResponses(snapshotRoot, firstKey, finalKey)}
	mgr := NewWithRunner(runner)
	sourceCalled := false
	applied := make([]string, 0, 2)

	result, err := mgr.EnsurePrismaDevDatabase(context.Background(), PrismaDevDatabaseOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		SourcePolicy: SourcePolicyFunc{
			PolicyName: "clone-dev",
			Fn: func(ctx context.Context, db api.DBInstance, opts PrepareOptions) error {
				sourceCalled = true
				return nil
			},
		},
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			applied = append(applied, migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sourceCalled {
		t.Fatal("expected source policy to rebuild base after older Prisma migration changed")
	}
	if strings.Join(applied, ",") != "001_init,002_users_name" {
		t.Fatalf("expected all Prisma migrations to replay after older migration changed, got %v", applied)
	}
	if result == nil || result.Base == nil || !result.Base.SourceApplied || result.SnapshotKey != finalKey || !result.Plan.ExactMatch {
		t.Fatalf("unexpected older Prisma migration change result: %+v", result)
	}
}

func TestEnsurePrismaDevDatabaseMigrationFailureDoesNotSnapshot(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key);\n")
	state, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	runner := &fakeRunner{responses: prismaRuntimeResponses(snapshotRoot)}
	mgr := NewWithRunner(runner)

	_, err = mgr.EnsurePrismaDevDatabase(context.Background(), PrismaDevDatabaseOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			return errors.New("migration failed")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "migration failed") {
		t.Fatalf("expected Prisma migration failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(snapshotRoot, PrismaSnapshotKey(state), "prisma.json")); !os.IsNotExist(statErr) {
		t.Fatalf("expected failed Prisma migration not to snapshot, stat error %v", statErr)
	}
	if runner.sawPrefix("docker stop -t 10 devflow-pg-abc") {
		t.Fatal("did not expect snapshot stop after failed Prisma migration")
	}
}

func TestEnsurePrismaDevDatabaseRejectsSchemaChangeWithoutMigration(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key);\n")
	oldState, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	if _, err := SavePrismaSnapshot(snapshotRoot, "old_schema", oldState); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(snapshotRoot, "old_schema", "volume.tgz"), "fake")
	if err := jsonWrite(filepath.Join(snapshotRoot, "old_schema", "manifest.json"), SnapshotManifest{
		Version:       1,
		Key:           "old_schema",
		Image:         DefaultPostgresImage,
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		Database:      "app_wt_abc",
		User:          "devflow",
		Port:          55432,
		ArchivePath:   filepath.Join(snapshotRoot, "old_schema", "volume.tgz"),
	}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id name String }\n")
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "rm", "-f", "devflow-pg-abc"):               {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "rm", "-f", "devflow-pgdata-abc"): {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):  {},
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/to", "-v", filepath.Join(snapshotRoot, "old_schema")+":/from", DefaultSidecarImage, "sh", "-c", "cd /to && tar xzf /from/volume.tgz"): {},
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {},
			portInspectKey("devflow-pg-abc"):                                       {},
			key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", "postgres:16.3"): {},
			key("docker", "exec", "devflow-pg-abc", "pg_isready", "-U", "devflow", "-d", "app_wt_abc"): {},
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
		SnapshotRoot:  snapshotRoot,
		URL:           "postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
	}
	_, err = mgr.EnsurePrismaDevDatabase(context.Background(), PrismaDevDatabaseOptions{
		Worktree:      worktree,
		DB:            db,
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
	})
	if err == nil || !strings.Contains(err.Error(), "schema changed without a new migration") {
		t.Fatalf("expected schema-without-migration error, got %v", err)
	}
	var migrationNeeded *MigrationNeededError
	if !errors.As(err, &migrationNeeded) || !migrationNeeded.MigrationNeeded() || migrationNeeded.Reason != "schema_changed" {
		t.Fatalf("expected schema-changed error to be marked migration-needed, got %#v", err)
	}
}

func TestEnsurePrismaDevDatabaseRejectsFreshSchemaWithModelsAndNoMigrations(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), `
datasource db {
  provider = "postgresql"
  url      = env("DATABASE_URL")
}

// model CommentedOut { id Int @id }
/*
model BlockCommentedOut {
  id Int @id
}
*/
model User {
  id Int @id @default(autoincrement())
}
`)
	snapshotRoot := t.TempDir()
	mgr := NewWithRunner(&fakeRunner{})
	_, err := mgr.EnsurePrismaDevDatabase(context.Background(), PrismaDevDatabaseOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
	})
	if err == nil || !strings.Contains(err.Error(), "no migrations exist") {
		t.Fatalf("expected fresh schema missing migration error, got %v", err)
	}
	var migrationNeeded *MigrationNeededError
	if !errors.As(err, &migrationNeeded) || !migrationNeeded.MigrationNeeded() || migrationNeeded.Reason != "no_migrations" {
		t.Fatalf("expected no-migrations error to be marked migration-needed, got %#v", err)
	}
	if _, statErr := os.Stat(snapshotRoot); statErr == nil {
		entries, readErr := os.ReadDir(snapshotRoot)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("expected no snapshot to be written for missing migrations, got %v", entries)
		}
	}
}

func TestPreparePrismaMigrationAuthoringDatabaseAllowsSchemaDriftForNewMigration(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key);\n")
	oldState, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	oldKey := PrismaSnapshotKey(oldState)
	writePrismaSnapshotFixture(t, snapshotRoot, oldKey, oldState)

	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id name String? }\n")
	responses := prismaRuntimeResponses(snapshotRoot)
	addPrismaRestoreResponse(responses, snapshotRoot, oldKey)
	runner := &fakeRunner{responses: responses}
	mgr := NewWithRunner(runner)

	result, err := mgr.PreparePrismaMigrationAuthoringDatabase(context.Background(), PrismaMigrationAuthoringOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			t.Fatalf("did not expect migration authoring prep to replay migration %s for schema-only drift", migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Applied {
		t.Fatalf("expected schema drift authoring prep to restore without applying tail, got %+v", result)
	}
	if result.Plan.ExactMatch || result.Plan.SnapshotKey != oldKey || result.Plan.PrefixLength != 1 {
		t.Fatalf("expected non-exact one-migration restore plan, got %+v", result.Plan)
	}
	if result.Base == nil || result.Base.Restored == nil {
		t.Fatalf("expected authoring prep to restore previous migration state, got %+v", result.Base)
	}
	if runner.sawPrefix("docker stop -t 10 devflow-pg-abc") {
		t.Fatal("did not expect authoring prep to snapshot schema-drift state")
	}
}

func TestPreparePrismaMigrationAuthoringDatabaseAllowsFreshModelSchemaWithoutMigrations(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), `
datasource db {
  provider = "postgresql"
  url      = env("DATABASE_URL")
}

model User {
  id Int @id @default(autoincrement())
}
`)
	snapshotRoot := t.TempDir()
	runner := &fakeRunner{responses: prismaRuntimeResponses(snapshotRoot)}
	mgr := NewWithRunner(runner)

	result, err := mgr.PreparePrismaMigrationAuthoringDatabase(context.Background(), PrismaMigrationAuthoringOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			t.Fatalf("did not expect fresh migration authoring prep to replay migration %s", migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Applied || len(result.State.Migrations) != 0 {
		t.Fatalf("unexpected fresh migration authoring result: %+v", result)
	}
	if result.Base == nil || !result.Base.Recreated {
		t.Fatalf("expected fresh migration authoring prep to recreate an empty runtime, got %+v", result.Base)
	}
	if runner.sawPrefix("docker stop -t 10 devflow-pg-abc") {
		t.Fatal("did not expect fresh migration authoring prep to snapshot")
	}
}

func TestPreparePrismaMigrationAuthoringDatabaseReplaysChangedLatestTail(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key);\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "002_role", "migration.sql"), "alter table users add column role text;\n")
	original, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	prefixState := prismaStatePrefix(original, 1)
	snapshotRoot := t.TempDir()
	prefixKey := PrismaSnapshotKey(prefixState)
	writePrismaSnapshotFixture(t, snapshotRoot, prefixKey, prefixState)

	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "002_role", "migration.sql"), "alter table users add column role text default 'rider';\n")
	responses := prismaRuntimeResponses(snapshotRoot)
	addPrismaRestoreResponse(responses, snapshotRoot, prefixKey)
	runner := &fakeRunner{responses: responses}
	mgr := NewWithRunner(runner)
	applied := make([]string, 0, 1)

	result, err := mgr.PreparePrismaMigrationAuthoringDatabase(context.Background(), PrismaMigrationAuthoringOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			applied = append(applied, migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(applied, ",") != "002_role" {
		t.Fatalf("expected authoring prep to apply only changed latest tail, got %v", applied)
	}
	if result == nil || !result.Applied || strings.Join(result.AppliedMigrations, ",") != "002_role" {
		t.Fatalf("unexpected authoring prep result: %+v", result)
	}
	if result.Base == nil || result.Base.Restored == nil || result.Base.Restored.Plan.PrefixLength != 1 {
		t.Fatalf("expected one-migration prefix restore, got %+v", result.Base)
	}
	if runner.sawPrefix("docker stop -t 10 devflow-pg-abc") {
		t.Fatal("did not expect authoring prep to write a migration-prefix snapshot")
	}
}

func TestPreparePrismaMigrationAuthoringDatabaseSourcePolicyAppliesAllMigrationsOnSnapshotMiss(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id role String? }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key);\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "002_role", "migration.sql"), "alter table users add column role text;\n")
	snapshotRoot := t.TempDir()
	runner := &fakeRunner{responses: prismaRuntimeResponses(snapshotRoot)}
	mgr := NewWithRunner(runner)
	sourceCalled := false
	applied := make([]string, 0, 2)

	result, err := mgr.PreparePrismaMigrationAuthoringDatabase(context.Background(), PrismaMigrationAuthoringOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		SourcePolicy: SourcePolicyFunc{
			PolicyName: "clone-dev",
			Fn: func(ctx context.Context, db api.DBInstance, opts PrepareOptions) error {
				sourceCalled = true
				return nil
			},
		},
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			applied = append(applied, migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sourceCalled {
		t.Fatal("expected source policy to run when authoring prep has no compatible snapshot")
	}
	if strings.Join(applied, ",") != "001_init,002_role" {
		t.Fatalf("expected authoring prep to apply all migrations after source rebuild, got %v", applied)
	}
	if result == nil || !result.Applied || result.Base == nil || !result.Base.SourceApplied || result.Base.SourcePolicy != "clone-dev" {
		t.Fatalf("unexpected source-policy authoring prep result: %+v", result)
	}
}

func TestPreparePrismaMigrationAuthoringDatabaseExactSnapshotIsNoOp(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id }\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key);\n")
	state, err := InspectPrismaState(worktree, "prisma/schema.prisma", "prisma/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	key := PrismaSnapshotKey(state)
	writePrismaSnapshotFixture(t, snapshotRoot, key, state)
	responses := prismaRuntimeResponses(snapshotRoot)
	addPrismaRestoreResponse(responses, snapshotRoot, key)
	runner := &fakeRunner{responses: responses}
	mgr := NewWithRunner(runner)

	result, err := mgr.PreparePrismaMigrationAuthoringDatabase(context.Background(), PrismaMigrationAuthoringOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		MigrateEach: func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
			t.Fatalf("did not expect exact authoring prep to replay migration %s", migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Applied || !result.Plan.ExactMatch || result.Plan.SnapshotKey != key {
		t.Fatalf("unexpected exact authoring prep result: %+v", result)
	}
	if result.Base == nil || result.Base.Restored == nil {
		t.Fatalf("expected exact authoring prep to restore the current snapshot, got %+v", result.Base)
	}
	if runner.sawPrefix("docker stop -t 10 devflow-pg-abc") {
		t.Fatal("did not expect exact authoring prep to snapshot")
	}
}

func TestPrismaCommandBuilders(t *testing.T) {
	deploy := PrismaMigrateDeployCommand("prisma/schema.prisma")
	if deploy.Name != "npx" || strings.Join(deploy.Args, " ") != "prisma migrate deploy --schema prisma/schema.prisma" {
		t.Fatalf("unexpected deploy command: %+v", deploy)
	}
	dev := PrismaMigrateDevCommand("prisma/schema.prisma", "add-user", true)
	if dev.Name != "npx" || strings.Join(dev.Args, " ") != "prisma migrate dev --name add-user --schema prisma/schema.prisma --create-only" {
		t.Fatalf("unexpected migrate dev command: %+v", dev)
	}
}

func TestGeneratePrismaMigrationRequiresName(t *testing.T) {
	err := GeneratePrismaMigration(context.Background(), PrismaMigrationGenerateOptions{Worktree: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "migration name is required") {
		t.Fatalf("expected missing migration name error, got %v", err)
	}
}

func TestGeneratePrismaMigrationRunsDefaultCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test is unix-only")
	}
	worktree := t.TempDir()
	binDir := filepath.Join(worktree, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteExecutable(t, filepath.Join(binDir, "npx"), "#!/bin/sh\nprintf '%s' \"$*\" > \"$OUT_FILE\"\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output := filepath.Join(worktree, "prisma-generate.txt")
	err := GeneratePrismaMigration(context.Background(), PrismaMigrationGenerateOptions{
		Worktree:   worktree,
		SchemaPath: "prisma/schema.prisma",
		Name:       "add-user",
		CreateOnly: true,
		Env:        map[string]string{"OUT_FILE": output},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(data))
	want := "prisma migrate dev --name add-user --schema prisma/schema.prisma --create-only"
	if got != want {
		t.Fatalf("unexpected generated Prisma command %q, want %q", got, want)
	}
}

func TestPrismaMigrateDeployPrefixApplierCopiesOnlyPrefix(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table a(id int);\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "002_users", "migration.sql"), "create table users(id int);\n")
	mustWrite(t, filepath.Join(worktree, "prisma", "migrations", "migration_lock.toml"), "provider = \"postgresql\"\n")
	binDir := filepath.Join(worktree, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteExecutable(t, filepath.Join(binDir, "npx"), `#!/bin/sh
schema="$5"
migrations="$(dirname "$schema")/migrations"
find "$migrations" -mindepth 1 -maxdepth 1 -type d | sort | wc -l | tr -d ' ' > "$OUT_FILE"
printf '%s' "$DATABASE_URL" > "$OUT_FILE.url"
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output := filepath.Join(worktree, "prefix-count.txt")
	applier := PrismaMigrateDeployPrefixApplier()
	err := applier(context.Background(), api.DBInstance{
		Name: "app_wt_abc",
		URL:  "postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
		Host: "127.0.0.1",
		Port: 55432,
		User: "devflow",
	}, PrismaMigration{Name: "001_init"}, PrismaMigrationApplyOptions{
		Worktree:      worktree,
		SchemaPath:    "prisma/schema.prisma",
		MigrationsDir: "prisma/migrations",
		Index:         0,
		Env:           map[string]string{"OUT_FILE": output},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "1" {
		t.Fatalf("expected one copied migration, got %q", string(data))
	}
	url, err := os.ReadFile(output + ".url")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(url)) == "" {
		t.Fatal("expected database env to be forwarded to prisma migrate deploy")
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

func writePrismaSnapshotFixture(t *testing.T, snapshotRoot, snapshotKey string, state *PrismaState) {
	t.Helper()
	if _, err := SavePrismaSnapshot(snapshotRoot, snapshotKey, state); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(snapshotRoot, snapshotKey, "volume.tgz"), "fake")
	if err := jsonWrite(filepath.Join(snapshotRoot, snapshotKey, "manifest.json"), SnapshotManifest{
		Version:       1,
		Key:           snapshotKey,
		Image:         DefaultPostgresImage,
		ContainerName: "devflow-pg-abc",
		VolumeName:    "devflow-pgdata-abc",
		Database:      "app_wt_abc",
		User:          "devflow",
		Port:          55432,
		ArchivePath:   filepath.Join(snapshotRoot, snapshotKey, "volume.tgz"),
	}); err != nil {
		t.Fatal(err)
	}
}

func prismaRuntimeResponses(snapshotRoot string, snapshotKeys ...string) map[string]response {
	responses := map[string]response{
		key("docker", "rm", "-f", "devflow-pg-abc"):                            {err: errors.New("Error: No such container: devflow-pg-abc")},
		key("docker", "volume", "rm", "-f", "devflow-pgdata-abc"):              {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
		key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {err: errors.New("Error: No such container: devflow-pg-abc")},
		key("docker", "volume", "inspect", "devflow-pgdata-abc"):               {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
		key("docker", "volume", "create", "devflow-pgdata-abc"):                {},
		key("docker", "run", "-d", "--name", "devflow-pg-abc", "--label", "devflow.managed=true", "--label", "devflow.database=true", "-p", "55432:5432", "-e", "POSTGRES_USER=devflow", "-e", "POSTGRES_PASSWORD=secret", "-e", "POSTGRES_DB=app_wt_abc", "-v", "devflow-pgdata-abc:/var/lib/postgresql/data", "postgres:16.3"): {},
		key("docker", "exec", "devflow-pg-abc", "pg_isready", "-U", "devflow", "-d", "app_wt_abc"): {},
		key("docker", "stop", "-t", "10", "devflow-pg-abc"):                                        {},
	}
	for _, snapshotKey := range snapshotKeys {
		responses[key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/from", "-v", filepath.Join(snapshotRoot, snapshotKey)+":/to", DefaultSidecarImage, "sh", "-c", "cd /from && tar czf /to/volume.tgz .")] = response{}
	}
	return responses
}

func addPrismaRestoreResponse(responses map[string]response, snapshotRoot, snapshotKey string) {
	responses[key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/to", "-v", filepath.Join(snapshotRoot, snapshotKey)+":/from", DefaultSidecarImage, "sh", "-c", "cd /to && tar xzf /from/volume.tgz")] = response{}
}
