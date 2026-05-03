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
