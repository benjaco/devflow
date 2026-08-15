package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

type PostgresComponent struct {
	name            string
	flavor          string
	postgresVersion int
	image           string
	sidecarImage    string
	host            string
	containerPort   int
	user            string
	password        string
	databaseName    string
	snapshotRoot    string
	portName        string

	bound bool
}

type PrismaComponent struct {
	name           string
	schemaPath     string
	migrationsDir  string
	basePaths      []string
	db             *PostgresComponent
	sourcePolicy   SourcePolicy
	sourceEnv      string
	sourceStrategy PostgresClientStrategy
	readyTimeout   time.Duration

	clientTask       *project.TaskBuilder
	migrationsTask   *project.TaskBuilder
	newMigrationTask *project.TaskBuilder
	migrationAction  *project.ActionBuilder
}

func Postgres(name string) *PostgresComponent {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "db"
	}
	return &PostgresComponent{
		name:         name,
		flavor:       FlavorPostgres,
		host:         "127.0.0.1",
		user:         "devflow",
		password:     "devflow",
		databaseName: name,
		portName:     name,
	}
}

func PostGIS(name string, postgresVersion int) *PostgresComponent {
	p := Postgres(name)
	p.flavor = FlavorPostGIS
	p.postgresVersion = postgresVersion
	return p
}

func (p *PostgresComponent) Image(image string) *PostgresComponent {
	p.image = image
	return p
}

func (p *PostgresComponent) SidecarImage(image string) *PostgresComponent {
	p.sidecarImage = image
	return p
}

func (p *PostgresComponent) ContainerPort(port int) *PostgresComponent {
	if port > 0 {
		p.containerPort = port
	}
	return p
}

func (p *PostgresComponent) User(user string) *PostgresComponent {
	p.user = user
	return p
}

func (p *PostgresComponent) Password(password string) *PostgresComponent {
	p.password = password
	return p
}

func (p *PostgresComponent) DatabaseName(name string) *PostgresComponent {
	p.databaseName = name
	return p
}

func (p *PostgresComponent) PortName(name string) *PostgresComponent {
	if name != "" {
		p.portName = name
	}
	return p
}

func (p *PostgresComponent) SnapshotRoot(path string) *PostgresComponent {
	p.snapshotRoot = path
	return p
}

func Prisma(name string) *PrismaComponent {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "prisma"
	}
	return &PrismaComponent{
		name:          name,
		schemaPath:    "prisma/schema.prisma",
		migrationsDir: "prisma/migrations",
		readyTimeout:  45 * time.Second,
	}
}

func (p *PrismaComponent) Schema(path string) *PrismaComponent {
	if path != "" {
		p.schemaPath = path
	}
	return p
}

func (p *PrismaComponent) MigrationDir(path string) *PrismaComponent {
	if path != "" {
		p.migrationsDir = path
	}
	return p
}

func (p *PrismaComponent) BasePaths(paths ...string) *PrismaComponent {
	p.basePaths = append(p.basePaths, paths...)
	return p
}

func (p *PrismaComponent) Database(db *PostgresComponent) *PrismaComponent {
	p.db = db
	return p
}

func (p *PrismaComponent) SourcePolicy(policy SourcePolicy) *PrismaComponent {
	p.sourcePolicy = policy
	return p
}

func (p *PrismaComponent) CloneFromEnv(key string) *PrismaComponent {
	p.sourceEnv = key
	return p
}

// CloneFromEnvContainerized clones through pg_dump and psql bundled in the
// managed Postgres image, so the host only needs a working Docker Engine.
func (p *PrismaComponent) CloneFromEnvContainerized(key string) *PrismaComponent {
	p.sourceEnv = key
	p.sourceStrategy = PostgresClientContainer
	return p
}

func (p *PrismaComponent) ReadyTimeout(timeout time.Duration) *PrismaComponent {
	p.readyTimeout = timeout
	return p
}

func (p *PrismaComponent) Client(b *project.Builder) *project.TaskBuilder {
	p.bind(b)
	if p.clientTask != nil {
		return p.clientTask
	}
	p.clientTask = b.Task(p.name+"_client").
		Command("npx", "prisma", "generate", "--schema", p.schemaPath).
		Inputs(p.schemaPath, "package.json", "package-lock.json").
		NoCache()
	return p.clientTask
}

func (p *PrismaComponent) Migrations(b *project.Builder) *project.TaskBuilder {
	p.bind(b)
	if p.migrationsTask != nil {
		return p.migrationsTask
	}
	p.migrationsTask = b.Task(p.name+"_migrations").
		Inputs(p.schemaPath, p.migrationsDir, "package.json", "package-lock.json").
		InputEnv("DATABASE_URL", p.sourceEnv).
		Run(func(ctx context.Context, rt *project.Runtime) error {
			result, err := EnsurePrismaDevDatabaseForRuntime(ctx, rt, PrismaDevDatabaseOptions{
				SchemaPath:    p.schemaPath,
				MigrationsDir: p.migrationsDir,
				BasePaths:     append([]string(nil), p.basePaths...),
				SourcePolicy:  p.sourcePolicyForRuntime(rt),
				ReadyTimeout:  p.readyTimeout,
			})
			if err != nil {
				rt.EmitLogLine("stderr", p.name+"_migrations failed: "+err.Error())
				return err
			}
			_ = rt.EmitJSONLine(p.name+"_migrations result", SummarizePrismaDevDatabase(result))
			return nil
		})
	p.migrationsTask.RequiredCLIs(append([]string{"npx"}, p.sourceRequiredCLIs()...)...)
	if p.sourceEnv != "" {
		p.migrationsTask.RequiredEnv(p.sourceEnv)
	}
	return p.migrationsTask
}

func (p *PrismaComponent) NewMigration(b *project.Builder) *project.TaskBuilder {
	p.bind(b)
	if p.newMigrationTask != nil {
		p.registerMigrationAction(b)
		return p.newMigrationTask
	}
	inputEnv := []string{"DEVFLOW_MIGRATION_NAME", "DATABASE_URL"}
	if p.sourceEnv != "" {
		inputEnv = append(inputEnv, p.sourceEnv)
	}
	p.newMigrationTask = b.Task(p.name+"_new_migration").
		Inputs(p.schemaPath, p.migrationsDir, "package.json", "package-lock.json").
		InputEnv(inputEnv...).
		Outputs(p.migrationsDir).
		NoCache().
		Run(func(ctx context.Context, rt *project.Runtime) error {
			name := strings.TrimSpace(rt.Env["DEVFLOW_MIGRATION_NAME"])
			if name == "" {
				name = strings.TrimSpace(os.Getenv("DEVFLOW_MIGRATION_NAME"))
			}
			if name == "" {
				return fmt.Errorf("DEVFLOW_MIGRATION_NAME is required")
			}
			result, err := PreparePrismaMigrationAuthoringDatabaseForRuntime(ctx, rt, PrismaMigrationAuthoringOptions{
				SchemaPath:    p.schemaPath,
				MigrationsDir: p.migrationsDir,
				BasePaths:     append([]string(nil), p.basePaths...),
				SourcePolicy:  p.sourcePolicyForRuntime(rt),
				ReadyTimeout:  p.readyTimeout,
			})
			if err != nil {
				rt.EmitLogLine("stderr", p.name+"_new_migration database failed: "+err.Error())
				return err
			}
			_ = rt.EmitJSONLine(p.name+"_new_migration database", SummarizePrismaMigrationAuthoring(result))
			if err := GeneratePrismaMigrationForRuntime(ctx, rt, PrismaMigrationGenerateOptions{
				SchemaPath: p.schemaPath,
				Name:       name,
				CreateOnly: true,
			}); err != nil {
				return err
			}
			rt.EmitLogLine("stdout", fmt.Sprintf("created Prisma migration %q", name))
			return nil
		})
	p.newMigrationTask.RequiredCLIs(append([]string{"npx"}, p.sourceRequiredCLIs()...)...)
	if p.sourceEnv != "" {
		p.newMigrationTask.RequiredEnv(p.sourceEnv)
	}
	p.registerMigrationAction(b)
	return p.newMigrationTask
}

func (p *PrismaComponent) registerMigrationAction(b *project.Builder) {
	if p.newMigrationTask == nil || p.migrationAction != nil {
		return
	}
	action := b.Action(p.name + ".migration.create")
	action.
		Kind(ActionMigrationCreate).
		Category(project.ActionCategoryAuthoring).
		Label("Create Prisma migration").
		Description("Generate a new Prisma migration file from schema changes.").
		Component(p.name).
		Task(p.newMigrationTask).
		Input(project.ActionInput{
			Name:        "name",
			Type:        project.ActionInputString,
			Label:       "Migration name",
			Required:    true,
			Positional:  true,
			Env:         "DEVFLOW_MIGRATION_NAME",
			Description: "Slug used by Prisma for the generated migration folder.",
		}).
		Writes(p.migrationsDir).
		Touches("database."+p.name).
		Invalidates(p.name+"_migrations", p.name+"_client").
		RelaunchPreviousTargetAfterSuccess().
		Alias(p.name + ":migration:create")
	p.migrationAction = action
}

func (p *PrismaComponent) bind(b *project.Builder) {
	if p.db == nil {
		p.db = Postgres(p.name)
	}
	p.db.bind(b)
	b.RequiredCLIs("npx")
	b.RequiredCLIs(p.sourceRequiredCLIs()...)
	b.PrismaConfig(project.PrismaConfig{
		SchemaPath:    p.schemaPath,
		MigrationsDir: p.migrationsDir,
		BasePaths:     append([]string(nil), p.basePaths...),
		CreateOnly:    true,
	})
}

func (p *PrismaComponent) sourceRequiredCLIs() []string {
	if p.sourceEnv != "" {
		if p.sourceStrategy == PostgresClientContainer {
			return nil
		}
		return []string{"pg_dump", "psql"}
	}
	switch policy := p.sourcePolicy.(type) {
	case PostgresDumpSourcePolicy:
		if policy.ClientStrategy == PostgresClientContainer {
			return nil
		}
		return []string{"pg_dump", "psql"}
	case *PostgresDumpSourcePolicy:
		if policy != nil && policy.ClientStrategy == PostgresClientContainer {
			return nil
		}
		return []string{"pg_dump", "psql"}
	default:
		return nil
	}
}

func (p *PrismaComponent) sourcePolicyForRuntime(rt *project.Runtime) SourcePolicy {
	if p.sourcePolicy != nil {
		return p.sourcePolicy
	}
	if p.sourceEnv == "" || rt == nil {
		return nil
	}
	remote := strings.TrimSpace(rt.Env[p.sourceEnv])
	if remote == "" {
		remote = strings.TrimSpace(os.Getenv(p.sourceEnv))
	}
	if remote == "" {
		return nil
	}
	return PostgresDumpSourcePolicy{
		PolicyName:     "clone-" + strings.ToLower(p.name),
		RemoteURL:      remote,
		ClientStrategy: p.sourceStrategy,
	}
}

func (p *PostgresComponent) bind(b *project.Builder) {
	if p == nil || p.bound {
		return
	}
	p.bound = true
	b.Port(p.portName)
	cfg := *p
	b.Finalize(func(inst *api.Instance) error {
		if cfg.flavor == FlavorPostGIS {
			if _, err := postGISRuntimeForVersion(cfg.postgresVersion); err != nil {
				return err
			}
		}
		manager := New()
		snapshotRoot := cfg.snapshotRoot
		if snapshotRoot == "" {
			snapshotRoot = filepath.Join(inst.Worktree, ".devflow", "db-snapshots")
		}
		databaseName := cfg.databaseName
		if databaseName == "" {
			databaseName = cfg.name
		}
		db := manager.Desired(inst.ID, Config{
			Flavor:          cfg.flavor,
			PostgresVersion: cfg.postgresVersion,
			Image:           cfg.image,
			SidecarImage:    cfg.sidecarImage,
			Host:            cfg.host,
			HostPort:        inst.Ports[cfg.portName],
			ContainerPort:   cfg.containerPort,
			Database:        sanitizeDBName(databaseName + "_" + inst.ID),
			User:            firstNonEmptyDatabase(cfg.user, "devflow"),
			Password:        firstNonEmptyDatabase(cfg.password, "devflow"),
			SnapshotRoot:    snapshotRoot,
		})
		inst.DB = db
		inst.Env = mergeEnvMaps(inst.Env, map[string]string{
			"PGHOST":       db.Host,
			"PGPORT":       fmt.Sprint(db.Port),
			"PGDATABASE":   db.Name,
			"PGUSER":       db.User,
			"PGPASSWORD":   db.Password,
			"DATABASE_URL": db.URL,
		})
		return nil
	})
}

func sanitizeDBName(value string) string {
	value = strings.ToLower(value)
	var out strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			out.WriteRune(r)
		} else {
			out.WriteRune('_')
		}
	}
	return strings.Trim(out.String(), "_")
}

func mergeEnvMaps(base, overlay map[string]string) map[string]string {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(overlay))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range overlay {
		out[key] = value
	}
	return out
}

func firstNonEmptyDatabase(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
