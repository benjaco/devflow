package database

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/process"
)

func TestPlanMigrationRestoreUsesNearestPrefix(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "001_init.sql"), "create table a(id int);\n")
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "002_users.sql"), "create table users(id int);\n")
	mustWrite(t, filepath.Join(worktree, "db", "seed.sql"), "-- seed\n")

	state, err := InspectMigrationState(worktree, "db/migrations", []string{"db/seed.sql"})
	if err != nil {
		t.Fatal(err)
	}
	prefixState := *state
	prefixState.Migrations = append([]MigrationPoint(nil), state.Migrations[:1]...)
	prefixState.FullHash = hashStrings([]string{
		"base:" + prefixState.BaseFingerprint,
		prefixState.Migrations[0].Name + ":" + prefixState.Migrations[0].Hash,
	})
	root := t.TempDir()
	if _, err := SaveMigrationSnapshot(root, "prefix_001", &prefixState); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanMigrationRestore(root, state)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ExactMatch || plan.SnapshotKey != "prefix_001" || plan.PrefixLength != 1 {
		t.Fatalf("unexpected migration restore plan: %+v", plan)
	}
}

func TestPlanMigrationRestoreIgnoresBaseFingerprintMismatch(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "001_init.sql"), "create table a(id int);\n")
	mustWrite(t, filepath.Join(worktree, "db", "seed.sql"), "-- old seed\n")
	oldState, err := InspectMigrationState(worktree, "db/migrations", []string{"db/seed.sql"})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := SaveMigrationSnapshot(root, "old_base", oldState); err != nil {
		t.Fatal(err)
	}

	mustWrite(t, filepath.Join(worktree, "db", "seed.sql"), "-- new seed\n")
	nextState, err := InspectMigrationState(worktree, "db/migrations", []string{"db/seed.sql"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanMigrationRestore(root, nextState)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SnapshotKey != "" {
		t.Fatalf("expected no reusable snapshot after base change, got %+v", plan)
	}
}

func TestEnsureMigratedDatabaseReusesExactSnapshotWithoutApplying(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "001_init.sql"), "create table a(id int);\n")
	state, err := InspectMigrationState(worktree, "db/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	finalKey := MigrationSnapshotKey(state)
	if _, err := SaveMigrationSnapshot(snapshotRoot, finalKey, state); err != nil {
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
	db := migrationTestDB(snapshotRoot)
	result, err := mgr.EnsureMigratedDatabase(context.Background(), ManagedMigrationOptions{
		Worktree:      worktree,
		DB:            db,
		MigrationsDir: "db/migrations",
		ApplyEach: func(ctx context.Context, db api.DBInstance, migration MigrationPoint, opts MigrationApplyOptions) error {
			t.Fatalf("did not expect exact snapshot to apply migration %s", migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Applied || !result.Plan.ExactMatch || result.Plan.SnapshotKey != finalKey {
		t.Fatalf("unexpected exact snapshot result: %+v", result)
	}
	if runner.sawPrefix("docker stop -t 10 devflow-pg-abc") {
		t.Fatal("did not expect final snapshot when exact snapshot was restored")
	}
}

func TestEnsureMigratedDatabaseRestoresPrefixAppliesAndSnapshots(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "001_init.sql"), "create table a(id int);\n")
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "002_users.sql"), "create table users(id int);\n")

	state, err := InspectMigrationState(worktree, "db/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	prefixState := *state
	prefixState.Migrations = append([]MigrationPoint(nil), state.Migrations[:1]...)
	prefixState.FullHash = hashStrings([]string{
		"base:" + prefixState.BaseFingerprint,
		prefixState.Migrations[0].Name + ":" + prefixState.Migrations[0].Hash,
	})
	snapshotRoot := t.TempDir()
	if _, err := SaveMigrationSnapshot(snapshotRoot, "prefix_001", &prefixState); err != nil {
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
	finalKey := MigrationSnapshotKey(state)
	applyMarker := filepath.Join(worktree, "applied.txt")
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
	result, err := mgr.EnsureMigratedDatabase(context.Background(), ManagedMigrationOptions{
		Worktree:      worktree,
		DB:            db,
		MigrationsDir: "db/migrations",
		Apply: process.CommandSpec{
			Name: "sh",
			Args: []string{"-c", "printf migrate > " + strconv.Quote(applyMarker)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.Applied || result.SnapshotKey != finalKey || !result.Plan.ExactMatch {
		t.Fatalf("unexpected managed migration result: %+v", result)
	}
	data, err := os.ReadFile(applyMarker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "migrate" {
		t.Fatalf("expected apply command to run, got %q", string(data))
	}
	if _, err := os.Stat(filepath.Join(snapshotRoot, finalKey, "migrations.json")); err != nil {
		t.Fatalf("expected final migration snapshot metadata: %v", err)
	}
}

func TestEnsureMigratedDatabaseChangedLatestMigrationAppliesOnlyTail(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "001_init.sql"), "create table a(id int);\n")
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "002_users.sql"), "create table users(id int);\n")
	original, err := InspectMigrationState(worktree, "db/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	prefixState := migrationStatePrefix(original, 1)
	snapshotRoot := t.TempDir()
	prefixKey := MigrationSnapshotKey(prefixState)
	if _, err := SaveMigrationSnapshot(snapshotRoot, prefixKey, prefixState); err != nil {
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

	mustWrite(t, filepath.Join(worktree, "db", "migrations", "002_users.sql"), "create table users(id int, name text);\n")
	current, err := InspectMigrationState(worktree, "db/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	finalKey := MigrationSnapshotKey(current)
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
	result, err := mgr.EnsureMigratedDatabase(context.Background(), ManagedMigrationOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		MigrationsDir: "db/migrations",
		ApplyEach: func(ctx context.Context, db api.DBInstance, migration MigrationPoint, opts MigrationApplyOptions) error {
			applied = append(applied, migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(applied, ",") != "002_users.sql" {
		t.Fatalf("expected only changed tail migration to apply, got %v", applied)
	}
	if result == nil || result.SnapshotKey != finalKey || !result.Plan.ExactMatch {
		t.Fatalf("unexpected changed-tail result: %+v", result)
	}
}

func TestEnsureMigratedDatabaseCanSnapshotEachMigrationPoint(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "001_init.sql"), "create table a(id int);\n")
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "002_users.sql"), "create table users(id int);\n")
	state, err := InspectMigrationState(worktree, "db/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	firstKey := MigrationSnapshotKey(migrationStatePrefix(state, 1))
	finalKey := MigrationSnapshotKey(migrationStatePrefix(state, 2))
	snapshotRoot := t.TempDir()
	runner := &fakeRunner{
		responses: map[string]response{
			key("docker", "rm", "-f", "devflow-pg-abc"):                            {err: errors.New("Error: No such container: devflow-pg-abc")},
			key("docker", "volume", "rm", "-f", "devflow-pgdata-abc"):              {err: errors.New("Error: No such volume: devflow-pgdata-abc")},
			key("docker", "inspect", "-f", "{{.State.Running}}", "devflow-pg-abc"): {},
			portInspectKey("devflow-pg-abc"):                                       {},
			key("docker", "volume", "inspect", "devflow-pgdata-abc"):               {},
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
	result, err := mgr.EnsureMigratedDatabase(context.Background(), ManagedMigrationOptions{
		Worktree:      worktree,
		DB:            db,
		MigrationsDir: "db/migrations",
		ApplyEach: func(ctx context.Context, db api.DBInstance, migration MigrationPoint, opts MigrationApplyOptions) error {
			applied = append(applied, migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(applied, ",") != "001_init.sql,002_users.sql" {
		t.Fatalf("unexpected applied migrations: %v", applied)
	}
	if result == nil || result.SnapshotKey != finalKey || !result.Plan.ExactMatch {
		t.Fatalf("unexpected per-migration result: %+v", result)
	}
	for _, key := range []string{firstKey, finalKey} {
		if _, err := os.Stat(filepath.Join(snapshotRoot, key, "migrations.json")); err != nil {
			t.Fatalf("expected migration point snapshot %s: %v", key, err)
		}
	}
}

func TestEnsureMigratedDatabaseUsesSourcePolicyWhenBaseChanged(t *testing.T) {
	worktree := t.TempDir()
	mustWrite(t, filepath.Join(worktree, "db", "migrations", "001_init.sql"), "create table a(id int);\n")
	mustWrite(t, filepath.Join(worktree, "db", "seed.sql"), "-- old seed\n")
	oldState, err := InspectMigrationState(worktree, "db/migrations", []string{"db/seed.sql"})
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	if _, err := SaveMigrationSnapshot(snapshotRoot, "old_base", oldState); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(worktree, "db", "seed.sql"), "-- new seed\n")
	current, err := InspectMigrationState(worktree, "db/migrations", []string{"db/seed.sql"})
	if err != nil {
		t.Fatal(err)
	}
	finalKey := MigrationSnapshotKey(current)
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
			key("docker", "run", "--rm", "-v", "devflow-pgdata-abc:/from", "-v", filepath.Join(snapshotRoot, finalKey)+":/to", DefaultSidecarImage, "sh", "-c", "cd /from && tar czf /to/volume.tgz ."): {},
		},
	}
	mgr := NewWithRunner(runner)
	sourceCalled := false
	applied := make([]string, 0, 1)
	result, err := mgr.EnsureMigratedDatabase(context.Background(), ManagedMigrationOptions{
		Worktree:      worktree,
		DB:            migrationTestDB(snapshotRoot),
		MigrationsDir: "db/migrations",
		BasePaths:     []string{"db/seed.sql"},
		SourcePolicy: SourcePolicyFunc{
			PolicyName: "clone-dev",
			Fn: func(ctx context.Context, db api.DBInstance, opts PrepareOptions) error {
				sourceCalled = true
				return nil
			},
		},
		ApplyEach: func(ctx context.Context, db api.DBInstance, migration MigrationPoint, opts MigrationApplyOptions) error {
			applied = append(applied, migration.Name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sourceCalled {
		t.Fatal("expected source policy to rebuild base after base fingerprint changed")
	}
	if strings.Join(applied, ",") != "001_init.sql" {
		t.Fatalf("expected migrations to replay from rebuilt base, got %v", applied)
	}
	if result == nil || result.Base == nil || !result.Base.SourceApplied || result.SnapshotKey != finalKey {
		t.Fatalf("unexpected rebuilt-base result: %+v", result)
	}
}

func migrationTestDB(snapshotRoot string) api.DBInstance {
	return api.DBInstance{
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
}
