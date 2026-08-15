package database

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

func TestPrismaComponentDefinesCommonTasksAndInstanceDB(t *testing.T) {
	p := project.Define(func(ctx context.Context, b *project.Builder) error {
		b.Name("demo")
		db := Postgres("prisma")
		prisma := Prisma("prisma").
			Schema("prisma/schema.prisma").
			MigrationDir("prisma/migrations").
			Database(db).
			CloneFromEnv("DEV_DATABASE_URL")
		client := prisma.Client(b)
		migrations := prisma.Migrations(b)
		app := b.Service("app").
			Command("node", "server.js").
			DependsOn(client, migrations).
			Env("PORT", b.Port("app")).
			ReadyHTTP("app", "/health", 200)
		prisma.NewMigration(b)
		b.Target("up", app)
		return nil
	})

	tasks := p.Tasks()
	if taskByName(tasks, "prisma_client").Kind != project.KindOnce {
		t.Fatalf("missing prisma client task")
	}
	migrations := taskByName(tasks, "prisma_migrations")
	if migrations.Kind != project.KindOnce {
		t.Fatalf("missing prisma migrations task")
	}
	if migrations.Cache {
		t.Fatal("database migration tasks should not be task-cacheable")
	}
	for _, cli := range []string{"npx", "pg_dump", "psql"} {
		if !stringSliceContainsDatabaseTest(migrations.RequiredCLIs, cli) {
			t.Fatalf("expected migrations required CLIs to include %q, got %+v", cli, migrations.RequiredCLIs)
		}
	}
	if stringSliceContainsDatabaseTest(migrations.RequiredCLIs, "docker") {
		t.Fatalf("Docker executable must not be required by Engine API tasks: %+v", migrations.RequiredCLIs)
	}
	newMigration := taskByName(tasks, "prisma_new_migration")
	if newMigration.Cache {
		t.Fatal("migration authoring task should not be cacheable")
	}
	if len(newMigration.Outputs.Paths) != 1 || newMigration.Outputs.Paths[0] != "prisma/migrations" {
		t.Fatalf("unexpected new migration outputs: %+v", newMigration.Outputs)
	}
	for _, cli := range []string{"npx", "pg_dump", "psql"} {
		if !stringSliceContainsDatabaseTest(newMigration.RequiredCLIs, cli) {
			t.Fatalf("expected migration authoring required CLIs to include %q, got %+v", cli, newMigration.RequiredCLIs)
		}
	}
	if stringSliceContainsDatabaseTest(newMigration.RequiredCLIs, "docker") {
		t.Fatalf("Docker executable must not be required by Engine API tasks: %+v", newMigration.RequiredCLIs)
	}
	for _, key := range []string{"DEVFLOW_MIGRATION_NAME", "DATABASE_URL", "DEV_DATABASE_URL"} {
		if !stringSliceContainsDatabaseTest(newMigration.Inputs.Env, key) {
			t.Fatalf("expected new migration input env to include %q, got %+v", key, newMigration.Inputs.Env)
		}
	}
	action := actionByIDDatabaseTest(project.Actions(p), "prisma.migration.create")
	if action.Kind != ActionMigrationCreate {
		t.Fatalf("unexpected Prisma migration action kind %q", action.Kind)
	}
	if action.Task != "prisma_new_migration" || action.Component != "prisma" {
		t.Fatalf("unexpected Prisma migration action: %+v", action)
	}
	if len(action.Inputs) == 0 || action.Inputs[0].Env != "DEVFLOW_MIGRATION_NAME" {
		t.Fatalf("expected Prisma migration action name input, got %+v", action.Inputs)
	}

	cfg, err := p.ConfigureInstance(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := &api.Instance{
		ID:       "abc123",
		Worktree: t.TempDir(),
		Ports:    map[string]int{"prisma": 15432, "app": 18080},
		Env:      map[string]string{},
	}
	if err := cfg.Finalize(inst); err != nil {
		t.Fatal(err)
	}
	if inst.DB.Name != "prisma_abc123" {
		t.Fatalf("unexpected DB name %q", inst.DB.Name)
	}
	if inst.DB.Port != 15432 {
		t.Fatalf("unexpected DB port %d", inst.DB.Port)
	}
	if inst.Env["DATABASE_URL"] == "" {
		t.Fatal("expected DATABASE_URL in instance env")
	}
}

func TestPrismaComponentContainerizedCloneDoesNotRequireHostPostgresClients(t *testing.T) {
	p := project.Define(func(ctx context.Context, b *project.Builder) error {
		b.Name("demo")
		prisma := Prisma("prisma").CloneFromEnvContainerized("DEV_DATABASE_URL")
		migrations := prisma.Migrations(b)
		b.Target("migrate", migrations)
		return nil
	})
	task := taskByName(p.Tasks(), "prisma_migrations")
	if !stringSliceContainsDatabaseTest(task.RequiredCLIs, "npx") {
		t.Fatalf("expected Prisma CLI requirement, got %+v", task.RequiredCLIs)
	}
	for _, cli := range []string{"pg_dump", "psql", "docker"} {
		if stringSliceContainsDatabaseTest(task.RequiredCLIs, cli) {
			t.Fatalf("containerized clone must not require host %q executable: %+v", cli, task.RequiredCLIs)
		}
	}
	if !stringSliceContainsDatabaseTest(task.RequiredEnv, "DEV_DATABASE_URL") {
		t.Fatalf("expected source URL requirement, got %+v", task.RequiredEnv)
	}
}

func TestPayloadCMSComponentDefinesMigrationTasks(t *testing.T) {
	p := project.Define(func(ctx context.Context, b *project.Builder) error {
		b.Name("payload-demo")
		db := Postgres("payload").PortName("postgres")
		payload := PayloadCMS("payload").
			Config("src/payload.config.ts").
			MigrationDir("src/migrations").
			Database(db).
			Command("npm", "run", "payload", "--")
		migrations := payload.Migrations(b)
		app := b.Service("app").
			Command("npm", "run", "dev").
			DependsOn(migrations).
			Env("PORT", b.Port("app")).
			ReadyHTTP("app", "/health", 200)
		payload.NewMigration(b)
		b.Target("up", app)
		return nil
	})

	migrations := taskByName(p.Tasks(), "payload_migrations")
	if migrations.Kind != project.KindOnce {
		t.Fatalf("missing payload migrations task")
	}
	if migrations.Cache {
		t.Fatal("PayloadCMS migration apply task should not be task-cacheable")
	}
	for _, path := range []string{"src/payload.config.ts", "src/migrations", "src/collections", "src/globals"} {
		if !stringSliceContainsDatabaseTest(migrations.Inputs.Paths, path) {
			t.Fatalf("expected migrations path inputs to include %q, got %+v", path, migrations.Inputs.Paths)
		}
	}
	for _, key := range []string{"DATABASE_URL"} {
		if !stringSliceContainsDatabaseTest(migrations.Inputs.Env, key) {
			t.Fatalf("expected migrations input env to include %q, got %+v", key, migrations.Inputs.Env)
		}
	}

	newMigration := taskByName(p.Tasks(), "payload_new_migration")
	if newMigration.Cache {
		t.Fatal("PayloadCMS migration authoring task should not be cacheable")
	}
	if len(newMigration.Outputs.Paths) != 1 || newMigration.Outputs.Paths[0] != "src/migrations" {
		t.Fatalf("unexpected new migration outputs: %+v", newMigration.Outputs)
	}
	for _, key := range []string{"DEVFLOW_MIGRATION_NAME", "DATABASE_URL", "DEVFLOW_PAYLOAD_FORCE_ACCEPT_WARNING"} {
		if !stringSliceContainsDatabaseTest(newMigration.Inputs.Env, key) {
			t.Fatalf("expected new migration input env to include %q, got %+v", key, newMigration.Inputs.Env)
		}
	}
	for _, cli := range []string{"npm"} {
		if !stringSliceContainsDatabaseTest(newMigration.RequiredCLIs, cli) {
			t.Fatalf("expected new migration required CLIs to include %q, got %+v", cli, newMigration.RequiredCLIs)
		}
	}
	if stringSliceContainsDatabaseTest(newMigration.RequiredCLIs, "docker") {
		t.Fatalf("Docker executable must not be required by Engine API tasks: %+v", newMigration.RequiredCLIs)
	}
	action := actionByIDDatabaseTest(project.Actions(p), "payload.migration.create")
	if action.Kind != ActionMigrationCreate {
		t.Fatalf("unexpected PayloadCMS migration action kind %q", action.Kind)
	}
	if action.Task != "payload_new_migration" || action.Component != "payload" {
		t.Fatalf("unexpected PayloadCMS migration action: %+v", action)
	}
	if len(action.Inputs) < 2 || action.Inputs[0].Env != "DEVFLOW_MIGRATION_NAME" || action.Inputs[1].Env != "DEVFLOW_PAYLOAD_FORCE_ACCEPT_WARNING" {
		t.Fatalf("expected PayloadCMS migration action inputs, got %+v", action.Inputs)
	}

	cfg, err := p.ConfigureInstance(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := &api.Instance{
		ID:       "abc123",
		Worktree: t.TempDir(),
		Ports:    map[string]int{"postgres": 15432, "app": 18080},
		Env:      map[string]string{},
	}
	if err := cfg.Finalize(inst); err != nil {
		t.Fatal(err)
	}
	if inst.DB.Name != "payload_abc123" {
		t.Fatalf("unexpected DB name %q", inst.DB.Name)
	}
	if inst.Env["DATABASE_URL"] == "" {
		t.Fatal("expected DATABASE_URL in instance env")
	}
}

func TestPostgresComponentPreservesCustomRuntimeImagesAndPort(t *testing.T) {
	p := project.Define(func(ctx context.Context, b *project.Builder) error {
		b.Name("custom-postgres")
		db := Postgres("db").
			Image("example/postgres:arm-ready").
			SidecarImage("example/tar:stable").
			ContainerPort(6432)
		prisma := Prisma("prisma").Database(db)
		b.Target("setup", prisma.Migrations(b))
		return nil
	})
	cfg, err := p.ConfigureInstance(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := &api.Instance{
		ID:       "abc123",
		Worktree: t.TempDir(),
		Ports:    map[string]int{"db": 15432},
		Env:      map[string]string{},
	}
	if err := cfg.Finalize(inst); err != nil {
		t.Fatal(err)
	}
	if inst.DB.Image != "example/postgres:arm-ready" || inst.DB.SidecarImage != "example/tar:stable" || inst.DB.ContainerPort != 6432 {
		t.Fatalf("unexpected custom database runtime: %+v", inst.DB)
	}
}

func TestPostGISComponentPersistsFlavorAndDefersArchitectureImageSelection(t *testing.T) {
	for _, postgresVersion := range []int{16, 17, 18} {
		t.Run(strconv.Itoa(postgresVersion), func(t *testing.T) {
			p := project.Define(func(ctx context.Context, b *project.Builder) error {
				b.Name("postgis-demo")
				db := PostGIS("geo", postgresVersion)
				prisma := Prisma("prisma").Database(db)
				b.Target("setup", prisma.Migrations(b))
				return nil
			})
			cfg, err := p.ConfigureInstance(context.Background(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			inst := &api.Instance{
				ID:       "abc123",
				Worktree: t.TempDir(),
				Ports:    map[string]int{"geo": 15432},
				Env:      map[string]string{},
			}
			if err := cfg.Finalize(inst); err != nil {
				t.Fatal(err)
			}
			if inst.DB.Flavor != FlavorPostGIS || inst.DB.PostgresVersion != postgresVersion {
				t.Fatalf("unexpected database flavor/version: %+v", inst.DB)
			}
			if inst.DB.Image != "" {
				t.Fatalf("expected runtime to select the default PostGIS image from Docker architecture, got %q", inst.DB.Image)
			}
			wantVolume := "devflow-pgdata-abc123-pg" + strconv.Itoa(postgresVersion)
			if inst.DB.ContainerName != "devflow-pg-abc123" || inst.DB.VolumeName != wantVolume {
				t.Fatalf("unexpected PostGIS runtime identity: %+v", inst.DB)
			}
		})
	}
}

func TestPostGISComponentRejectsUnsupportedPostgresVersionDuringFinalization(t *testing.T) {
	p := project.Define(func(ctx context.Context, b *project.Builder) error {
		b.Name("invalid-postgis-version")
		db := PostGIS("geo", 15)
		b.Target("setup", Prisma("prisma").Database(db).Migrations(b))
		return nil
	})
	cfg, err := p.ConfigureInstance(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Finalize(&api.Instance{
		ID:       "abc123",
		Worktree: t.TempDir(),
		Ports:    map[string]int{"geo": 15432},
	})
	if err == nil || !strings.Contains(err.Error(), "supported versions are 16, 17, and 18") {
		t.Fatalf("expected unsupported PostGIS version error, got %v", err)
	}
}

func stringSliceContainsDatabaseTest(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func taskByName(tasks []project.Task, name string) project.Task {
	for _, task := range tasks {
		if task.Name == name {
			return task
		}
	}
	panic("missing task " + name)
}

func actionByIDDatabaseTest(actions []project.Action, id string) project.Action {
	for _, action := range actions {
		if action.ID == id {
			return action
		}
	}
	panic("missing action " + id)
}
