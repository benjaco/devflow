package database

import (
	"context"
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
		b.Target("up", app)
		b.Target("new-migration", prisma.NewMigration(b))
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
	newMigration := taskByName(tasks, "prisma_new_migration")
	if newMigration.Cache {
		t.Fatal("migration authoring task should not be cacheable")
	}
	if len(newMigration.Outputs.Paths) != 1 || newMigration.Outputs.Paths[0] != "prisma/migrations" {
		t.Fatalf("unexpected new migration outputs: %+v", newMigration.Outputs)
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

func taskByName(tasks []project.Task, name string) project.Task {
	for _, task := range tasks {
		if task.Name == name {
			return task
		}
	}
	panic("missing task " + name)
}
