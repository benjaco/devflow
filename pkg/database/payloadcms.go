package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/benjaco/devflow/internal/jsonutil"
	"github.com/benjaco/devflow/pkg/fingerprint"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

const PayloadSchemaPushEnv = "PAYLOAD_SCHEMA_PUSH"

const payloadSchemaPushStateVersion = 1

type PayloadCMSComponent struct {
	name          string
	configPath    string
	migrationsDir string
	schemaInputs  []any
	packageInputs []any
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
	devServices      map[*project.TaskBuilder]bool
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
		schemaInputs:  []any{"src/collections", "src/globals", "src/fields"},
		packageInputs: []any{"package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb"},
		commandName:   "npx",
		commandArgs:   []string{"payload"},
		migrationEnv:  "DEVFLOW_MIGRATION_NAME",
		forceEnv:      "DEVFLOW_PAYLOAD_FORCE_ACCEPT_WARNING",
		readyTimeout:  45 * time.Second,
		prompts:       defaultPayloadPrompts(),
		devServices:   map[*project.TaskBuilder]bool{},
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

// PackageInputs replaces the dependency manifests and lockfiles that can
// affect Payload's generated database schema. The defaults cover package.json
// and the common npm, pnpm, Yarn, and Bun lockfiles.
func (p *PayloadCMSComponent) PackageInputs(inputs ...any) *PayloadCMSComponent {
	p.packageInputs = append([]any(nil), inputs...)
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

// ConfigureDevService makes a Payload development service restart on schema
// inputs and sets PAYLOAD_SCHEMA_PUSH for each start. The applied fingerprint
// is persisted only after the service passes readiness.
func (p *PayloadCMSComponent) ConfigureDevService(service *project.TaskBuilder) *project.TaskBuilder {
	if p == nil || service == nil || p.devServices[service] {
		return service
	}
	p.devServices[service] = true
	inputs := p.schemaPushInputs()
	service.
		Inputs(inputs...).
		InputEnv("DATABASE_URL").
		BeforeRun(p.prepareSchemaPush).
		AfterReady(p.commitSchemaPush)
	return service
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
			runner := project.CommandOutputTasklet{
				Command:         spec,
				RequiredFiles:   []string{path.Join(filepath.ToSlash(p.migrationsDir), "**", "*")},
				RequireNewFiles: true,
			}
			if err := runner.Run(ctx, rt); err != nil {
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
	inputs := []any{p.configPath, p.migrationsDir}
	inputs = append(inputs, p.packageInputs...)
	inputs = append(inputs, p.schemaInputs...)
	return inputs
}

func (p *PayloadCMSComponent) schemaPushInputs() []any {
	inputs := []any{p.configPath}
	inputs = append(inputs, p.schemaInputs...)
	inputs = append(inputs, p.packageInputs...)
	return inputs
}

type payloadSchemaPushState struct {
	Version     int       `json:"version"`
	Fingerprint string    `json:"fingerprint"`
	Push        bool      `json:"push,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func (p *PayloadCMSComponent) prepareSchemaPush(ctx context.Context, rt *project.Runtime) error {
	if rt == nil || rt.Instance == nil || strings.TrimSpace(rt.Instance.ID) == "" {
		return fmt.Errorf("prepare PayloadCMS schema push: runtime instance is required")
	}
	current, err := p.schemaPushFingerprint(ctx, rt)
	if err != nil {
		return fmt.Errorf("prepare PayloadCMS schema push: %w", err)
	}
	applied, ok := p.loadSchemaPushState(p.schemaPushAppliedPath(rt))
	push := !ok || applied.Version != payloadSchemaPushStateVersion || applied.Fingerprint != current
	rt.Env[PayloadSchemaPushEnv] = fmt.Sprintf("%t", push)
	pending := payloadSchemaPushState{
		Version:     payloadSchemaPushStateVersion,
		Fingerprint: current,
		Push:        push,
		UpdatedAt:   time.Now().UTC(),
	}
	if err := jsonutil.WriteFileAtomic(p.schemaPushPendingPath(rt), pending); err != nil {
		return fmt.Errorf("record pending PayloadCMS schema fingerprint: %w", err)
	}
	if push {
		rt.EmitLogLine("stdout", "PayloadCMS schema changed; starting with PAYLOAD_SCHEMA_PUSH=true")
	} else {
		rt.EmitLogLine("stdout", "PayloadCMS schema unchanged; starting with PAYLOAD_SCHEMA_PUSH=false")
	}
	return nil
}

func (p *PayloadCMSComponent) commitSchemaPush(_ context.Context, rt *project.Runtime) error {
	pendingPath := p.schemaPushPendingPath(rt)
	pending, ok := p.loadSchemaPushState(pendingPath)
	if !ok || pending.Version != payloadSchemaPushStateVersion || pending.Fingerprint == "" {
		return fmt.Errorf("pending PayloadCMS schema fingerprint is missing or invalid")
	}
	if pending.Push {
		pending.Push = false
		pending.UpdatedAt = time.Now().UTC()
		if err := jsonutil.WriteFileAtomic(p.schemaPushAppliedPath(rt), pending); err != nil {
			return fmt.Errorf("commit applied PayloadCMS schema fingerprint: %w", err)
		}
		rt.EmitLogLine("stdout", "committed applied PayloadCMS schema fingerprint after readiness")
	}
	if err := os.Remove(pendingPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove pending PayloadCMS schema fingerprint: %w", err)
	}
	return nil
}

func (p *PayloadCMSComponent) schemaPushFingerprint(ctx context.Context, rt *project.Runtime) (string, error) {
	if rt == nil || strings.TrimSpace(rt.Worktree) == "" {
		return "", fmt.Errorf("runtime worktree is required")
	}
	identity, err := payloadDatabaseIdentity(rt.Env["DATABASE_URL"])
	if err != nil {
		return "", err
	}
	task := project.Task{Inputs: project.InputsFrom(p.schemaPushInputs()...)}
	hashes, _, err := fingerprint.CollectTaskStaticInputsWithCache(ctx, rt.Worktree, task, rt, nil)
	if err != nil {
		return "", err
	}
	return fingerprint.StaticInputDigest(hashes, []string{
		"payload-schema-push-v1",
		"database=" + identity,
	}), nil
}

func payloadDatabaseIdentity(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("DATABASE_URL is required for PayloadCMS schema push")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return "", fmt.Errorf("DATABASE_URL must be a valid postgres or postgresql URL")
	}
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	databaseName, err := url.PathUnescape(strings.TrimPrefix(parsed.EscapedPath(), "/"))
	if err != nil {
		return "", fmt.Errorf("DATABASE_URL contains an invalid database name")
	}
	username := ""
	if parsed.User != nil {
		username = parsed.User.Username()
	}
	query := parsed.Query()
	for key := range query {
		switch strings.ToLower(key) {
		case "password", "passfile", "sslpassword", "oauth_client_secret":
			query.Del(key)
		}
	}
	values := make([]string, 0, len(query))
	for key, entries := range query {
		sorted := append([]string(nil), entries...)
		sort.Strings(sorted)
		for _, value := range sorted {
			values = append(values, key+"="+value)
		}
	}
	sort.Strings(values)
	identity, err := json.Marshal(struct {
		Scheme   string   `json:"scheme"`
		Host     string   `json:"host"`
		Port     string   `json:"port"`
		Database string   `json:"database"`
		User     string   `json:"user,omitempty"`
		Options  []string `json:"options,omitempty"`
	}{
		Scheme:   parsed.Scheme,
		Host:     strings.ToLower(parsed.Hostname()),
		Port:     port,
		Database: databaseName,
		User:     username,
		Options:  values,
	})
	if err != nil {
		return "", fmt.Errorf("encode PayloadCMS database identity: %w", err)
	}
	return string(identity), nil
}

func (p *PayloadCMSComponent) loadSchemaPushState(path string) (payloadSchemaPushState, bool) {
	var state payloadSchemaPushState
	if err := jsonutil.ReadFile(path, &state); err != nil {
		return payloadSchemaPushState{}, false
	}
	return state, true
}

func (p *PayloadCMSComponent) schemaPushAppliedPath(rt *project.Runtime) string {
	return filepath.Join(p.schemaPushStateDir(rt), "applied.json")
}

func (p *PayloadCMSComponent) schemaPushPendingPath(rt *project.Runtime) string {
	task := "service"
	if rt != nil && strings.TrimSpace(rt.TaskName) != "" {
		task = rt.TaskName
	}
	sum := sha256.Sum256([]byte(task))
	return filepath.Join(p.schemaPushStateDir(rt), "pending-"+hex.EncodeToString(sum[:8])+".json")
}

func (p *PayloadCMSComponent) schemaPushStateDir(rt *project.Runtime) string {
	instanceID := "unknown"
	worktree := ""
	if rt != nil {
		worktree = rt.Worktree
		if rt.Instance != nil && strings.TrimSpace(rt.Instance.ID) != "" {
			instanceID = rt.Instance.ID
		}
	}
	sum := sha256.Sum256([]byte(p.name))
	return filepath.Join(worktree, ".devflow", "state", "instances", instanceID, "payload-schema", hex.EncodeToString(sum[:8]))
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
