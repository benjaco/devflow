package database

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

type PayloadCMSComponent struct {
	name          string
	configPath    string
	migrationsDir string
	schemaInputs  []any
	commandName   string
	commandArgs   []string
	migrationEnv  string
	forceEnv      string
	readyTimeout  time.Duration
	db            *PostgresComponent
	prompts       []process.PromptSpec

	migrationsTask   *project.TaskBuilder
	newMigrationTask *project.TaskBuilder
	migrationAction  *project.ActionBuilder
}

func PayloadCMS(name string) *PayloadCMSComponent {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "payload"
	}
	return &PayloadCMSComponent{
		name:          name,
		configPath:    "src/payload.config.ts",
		migrationsDir: "src/migrations",
		schemaInputs:  []any{"src/collections", "src/globals"},
		commandName:   "npx",
		commandArgs:   []string{"payload"},
		migrationEnv:  "DEVFLOW_MIGRATION_NAME",
		forceEnv:      "DEVFLOW_PAYLOAD_FORCE_ACCEPT_WARNING",
		readyTimeout:  45 * time.Second,
		prompts:       defaultPayloadPrompts(),
	}
}

func (p *PayloadCMSComponent) Config(path string) *PayloadCMSComponent {
	if path != "" {
		p.configPath = path
	}
	return p
}

func (p *PayloadCMSComponent) MigrationDir(path string) *PayloadCMSComponent {
	if path != "" {
		p.migrationsDir = path
	}
	return p
}

func (p *PayloadCMSComponent) SchemaInputs(inputs ...any) *PayloadCMSComponent {
	p.schemaInputs = append([]any(nil), inputs...)
	return p
}

func (p *PayloadCMSComponent) AddSchemaInputs(inputs ...any) *PayloadCMSComponent {
	p.schemaInputs = append(p.schemaInputs, inputs...)
	return p
}

func (p *PayloadCMSComponent) Database(db *PostgresComponent) *PayloadCMSComponent {
	p.db = db
	return p
}

func (p *PayloadCMSComponent) Command(name string, args ...string) *PayloadCMSComponent {
	if name != "" {
		p.commandName = name
		p.commandArgs = append([]string(nil), args...)
	}
	return p
}

func (p *PayloadCMSComponent) MigrationNameEnv(key string) *PayloadCMSComponent {
	if key != "" {
		p.migrationEnv = key
	}
	return p
}

func (p *PayloadCMSComponent) ForceAcceptWarningsEnv(key string) *PayloadCMSComponent {
	if key != "" {
		p.forceEnv = key
	}
	return p
}

func (p *PayloadCMSComponent) ReadyTimeout(timeout time.Duration) *PayloadCMSComponent {
	p.readyTimeout = timeout
	return p
}

func (p *PayloadCMSComponent) Prompts(prompts ...process.PromptSpec) *PayloadCMSComponent {
	p.prompts = append([]process.PromptSpec(nil), prompts...)
	return p
}

func (p *PayloadCMSComponent) AddPrompts(prompts ...process.PromptSpec) *PayloadCMSComponent {
	p.prompts = append(p.prompts, prompts...)
	return p
}

func (p *PayloadCMSComponent) Migrations(b *project.Builder) *project.TaskBuilder {
	p.bind(b)
	if p.migrationsTask != nil {
		return p.migrationsTask
	}
	p.migrationsTask = b.Task(p.name + "_migrations").
		Inputs(p.taskInputs()...).
		InputEnv("DATABASE_URL").
		NoCache().
		Run(func(ctx context.Context, rt *project.Runtime) error {
			if err := p.ensureRuntime(ctx, rt); err != nil {
				rt.EmitLogLine("stderr", p.name+"_migrations database failed: "+err.Error())
				return err
			}
			spec := p.commandSpec("migrate")
			if err := rt.RunCmdSpec(ctx, spec); err != nil {
				rt.EmitLogLine("stderr", p.name+"_migrations failed: "+err.Error())
				return err
			}
			rt.EmitLogLine("stdout", "applied PayloadCMS migrations")
			return nil
		})
	p.requiredCLIs(p.migrationsTask)
	return p.migrationsTask
}

func (p *PayloadCMSComponent) NewMigration(b *project.Builder) *project.TaskBuilder {
	p.bind(b)
	if p.newMigrationTask != nil {
		p.registerMigrationAction(b)
		return p.newMigrationTask
	}
	p.newMigrationTask = b.Task(p.name+"_new_migration").
		Inputs(p.taskInputs()...).
		InputEnv(p.migrationEnv, "DATABASE_URL", p.forceEnv).
		Outputs(p.migrationsDir).
		NoCache().
		Run(func(ctx context.Context, rt *project.Runtime) error {
			name := strings.TrimSpace(rt.Env[p.migrationEnv])
			if name == "" {
				name = strings.TrimSpace(os.Getenv(p.migrationEnv))
			}
			if name == "" {
				return fmt.Errorf("%s is required", p.migrationEnv)
			}
			if err := p.ensureRuntime(ctx, rt); err != nil {
				rt.EmitLogLine("stderr", p.name+"_new_migration database failed: "+err.Error())
				return err
			}
			args := []string{"migrate:create", name}
			if isTruthy(firstNonEmptyDatabase(rt.Env[p.forceEnv], os.Getenv(p.forceEnv))) {
				args = append(args, "--force-accept-warning")
			}
			spec := p.commandSpec(args...)
			spec.Interactive = len(p.prompts) > 0
			spec.Prompts = append([]process.PromptSpec(nil), p.prompts...)
			if err := rt.RunCmdSpec(ctx, spec); err != nil {
				rt.EmitLogLine("stderr", p.name+"_new_migration failed: "+err.Error())
				return err
			}
			rt.EmitLogLine("stdout", fmt.Sprintf("created PayloadCMS migration %q", name))
			return nil
		})
	p.requiredCLIs(p.newMigrationTask)
	p.registerMigrationAction(b)
	return p.newMigrationTask
}

func (p *PayloadCMSComponent) taskInputs() []any {
	inputs := []any{p.configPath, p.migrationsDir, "package.json", "package-lock.json"}
	inputs = append(inputs, p.schemaInputs...)
	return inputs
}

func (p *PayloadCMSComponent) registerMigrationAction(b *project.Builder) {
	if p.newMigrationTask == nil || p.migrationAction != nil {
		return
	}
	action := b.Action(p.name + ".migration.create")
	action.
		Kind(ActionMigrationCreate).
		Category(project.ActionCategoryAuthoring).
		Label("Create PayloadCMS migration").
		Description("Generate a new PayloadCMS migration file from config/schema changes.").
		Component(p.name).
		Task(p.newMigrationTask).
		Input(project.ActionInput{
			Name:        "name",
			Type:        project.ActionInputString,
			Label:       "Migration name",
			Required:    true,
			Positional:  true,
			Env:         p.migrationEnv,
			Description: "Name passed to PayloadCMS migrate:create.",
		}).
		Input(project.ActionInput{
			Name:        "force",
			Type:        project.ActionInputBool,
			Label:       "Accept warnings",
			Env:         p.forceEnv,
			Description: "Pass PayloadCMS force-accept-warning behavior for confirmed destructive prompts.",
		}).
		Writes(p.migrationsDir).
		Touches("database." + p.name).
		Invalidates(p.name + "_migrations").
		RelaunchPreviousTargetAfterSuccess().
		Alias(p.name + ":migration:create")
	p.migrationAction = action
}

func (p *PayloadCMSComponent) bind(b *project.Builder) {
	if p.db != nil {
		p.db.bind(b)
	}
	if p.commandName != "" {
		b.RequiredCLIs(p.commandName)
	}
}

func (p *PayloadCMSComponent) requiredCLIs(task *project.TaskBuilder) {
	if p.commandName != "" {
		task.RequiredCLIs(p.commandName)
	}
	if p.db != nil {
		task.RequiredCLIs("docker")
	}
}

func (p *PayloadCMSComponent) ensureRuntime(ctx context.Context, rt *project.Runtime) error {
	if p.db == nil || rt == nil || rt.Instance == nil || rt.Instance.DB.Name == "" {
		return nil
	}
	manager := New()
	if err := manager.EnsureRuntime(ctx, rt.Instance.DB); err != nil {
		return err
	}
	return manager.WaitReady(ctx, rt.Instance.DB, p.readyTimeout)
}

func (p *PayloadCMSComponent) commandSpec(args ...string) process.CommandSpec {
	allArgs := append([]string(nil), p.commandArgs...)
	allArgs = append(allArgs, args...)
	return process.CommandSpec{
		Name: p.commandName,
		Args: allArgs,
	}
}

func defaultPayloadPrompts() []process.PromptSpec {
	return []process.PromptSpec{
		{
			Patterns: []string{
				"DATA LOSS WARNING",
				"data loss",
				"Accept warnings and create migration? [y/N]: ",
				"Accept warnings and push schema to database? [y/N]: ",
				"Continue? [y/N]: ",
				"Are you sure you want to continue? [y/N]: ",
			},
			Prompt: "Accept PayloadCMS migration warning?",
			Kind:   process.PromptConfirm,
			Repeat: true,
		},
	}
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}
