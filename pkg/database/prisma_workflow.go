package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/process"
)

type PrismaDevDatabaseOptions struct {
	Worktree      string
	DB            api.DBInstance
	SchemaPath    string
	MigrationsDir string
	BasePaths     []string
	SourcePolicy  SourcePolicy
	Prepare       PrepareOptions
	Migrate       process.CommandSpec
	MigrateEach   PrismaMigrationApplyFunc
	SnapshotKey   string
	ReadyTimeout  time.Duration
}

type PrismaDevDatabaseResult struct {
	State       *PrismaState         `json:"state,omitempty"`
	Base        *PrismaBaseResult    `json:"base,omitempty"`
	Applied     bool                 `json:"applied"`
	Snapshot    *PrismaRestoreResult `json:"snapshot,omitempty"`
	SnapshotKey string               `json:"snapshotKey,omitempty"`
	Plan        PrismaRestorePlan    `json:"plan"`
}

type PrismaMigrationGenerateOptions struct {
	Worktree   string
	SchemaPath string
	Name       string
	CreateOnly bool
	Env        map[string]string
	LogPath    string
	OnLine     func(stream, line string)
	Command    process.CommandSpec
}

type PrismaMigrationAuthoringOptions struct {
	Worktree      string
	DB            api.DBInstance
	SchemaPath    string
	MigrationsDir string
	BasePaths     []string
	SourcePolicy  SourcePolicy
	Prepare       PrepareOptions
	MigrateEach   PrismaMigrationApplyFunc
	ReadyTimeout  time.Duration
}

type PrismaMigrationAuthoringResult struct {
	State             *PrismaState      `json:"state,omitempty"`
	Base              *PrismaBaseResult `json:"base,omitempty"`
	Plan              PrismaRestorePlan `json:"plan"`
	Applied           bool              `json:"applied"`
	AppliedMigrations []string          `json:"appliedMigrations,omitempty"`
}

type MigrationNeededError struct {
	Reason  string
	Message string
}

func (e *MigrationNeededError) Error() string {
	return e.Message
}

func (e *MigrationNeededError) MigrationNeeded() bool {
	return true
}

func newMigrationNeededError(reason, message string) error {
	return &MigrationNeededError{Reason: reason, Message: message}
}

type PrismaMigrationApplyFunc func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error

type PrismaMigrationApplyOptions struct {
	Worktree      string
	SchemaPath    string
	MigrationsDir string
	Index         int
	LogPath       string
	OnLine        func(stream, line string)
	Env           map[string]string
}

func (m *Manager) EnsurePrismaDevDatabase(ctx context.Context, opts PrismaDevDatabaseOptions) (*PrismaDevDatabaseResult, error) {
	state, err := InspectPrismaState(opts.Worktree, opts.SchemaPath, opts.MigrationsDir, opts.BasePaths)
	if err != nil {
		return nil, err
	}
	if len(state.Migrations) == 0 {
		hasModels, err := prismaSchemaDeclaresModels(opts.Worktree, opts.SchemaPath)
		if err != nil {
			return nil, err
		}
		if hasModels {
			return nil, newMigrationNeededError("no_migrations", "prisma schema declares models but no migrations exist; generate one with GeneratePrismaMigration before preparing the database")
		}
	}
	base, err := m.PreparePrismaBase(ctx, opts.DB, state, opts.SourcePolicy, opts.Prepare)
	if err != nil {
		return nil, err
	}
	emitPrepareLine(opts.Prepare, "stdout", "database: starting managed Postgres runtime")
	if err := m.EnsureRuntime(ctx, opts.DB); err != nil {
		return nil, err
	}
	timeout := opts.ReadyTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	emitPrepareLine(opts.Prepare, "stdout", "database: waiting for managed Postgres readiness")
	if err := m.WaitReady(ctx, opts.DB, timeout); err != nil {
		return nil, err
	}
	plan := PrismaRestorePlan{}
	if base != nil && base.Restored != nil {
		plan = base.Restored.Plan
	}
	result := &PrismaDevDatabaseResult{State: state, Base: base, Plan: plan}
	needsApply := plan.SnapshotKey == "" || !plan.ExactMatch
	if needsApply && len(state.Migrations) > 0 {
		if opts.Migrate.Name == "" {
			if plan.SnapshotKey != "" && plan.PrefixLength == len(state.Migrations) && plan.Snapshot != nil && plan.Snapshot.SchemaHash != state.SchemaHash {
				return nil, newMigrationNeededError("schema_changed", "prisma schema changed without a new migration; generate one with GeneratePrismaMigration before preparing the database")
			}
			applier := opts.MigrateEach
			if applier == nil {
				applier = PrismaMigrateDeployPrefixApplier()
			}
			snapshot, err := m.applyAndSnapshotEachPrismaMigration(ctx, opts, state, plan.PrefixLength, timeout, applier)
			if err != nil {
				return nil, err
			}
			result.Applied = plan.PrefixLength < len(state.Migrations)
			result.Snapshot = snapshot
			if snapshot != nil {
				result.SnapshotKey = snapshot.Plan.SnapshotKey
				result.Plan = snapshot.Plan
			}
			return result, nil
		}
		if err := runDatabaseCommand(ctx, opts.Migrate, opts.Worktree, opts.Prepare, opts.DB); err != nil {
			return nil, err
		}
		result.Applied = true
	}
	if needsApply {
		key := opts.SnapshotKey
		if key == "" {
			key = PrismaSnapshotKey(state)
		}
		snapshot, err := m.SnapshotPrisma(ctx, opts.DB, key, state)
		if err != nil {
			return nil, err
		}
		result.Snapshot = snapshot
		result.SnapshotKey = key
		result.Plan = snapshot.Plan
		if err := m.EnsureRuntime(ctx, opts.DB); err != nil {
			return nil, err
		}
		emitPrepareLine(opts.Prepare, "stdout", "database: waiting for managed Postgres readiness after snapshot")
		if err := m.WaitReady(ctx, opts.DB, timeout); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (m *Manager) PreparePrismaMigrationAuthoringDatabase(ctx context.Context, opts PrismaMigrationAuthoringOptions) (*PrismaMigrationAuthoringResult, error) {
	state, err := InspectPrismaState(opts.Worktree, opts.SchemaPath, opts.MigrationsDir, opts.BasePaths)
	if err != nil {
		return nil, err
	}
	base, err := m.PreparePrismaBase(ctx, opts.DB, state, opts.SourcePolicy, opts.Prepare)
	if err != nil {
		return nil, err
	}
	emitPrepareLine(opts.Prepare, "stdout", "database: starting managed Postgres runtime")
	if err := m.EnsureRuntime(ctx, opts.DB); err != nil {
		return nil, err
	}
	timeout := opts.ReadyTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	emitPrepareLine(opts.Prepare, "stdout", "database: waiting for managed Postgres readiness")
	if err := m.WaitReady(ctx, opts.DB, timeout); err != nil {
		return nil, err
	}
	plan := PrismaRestorePlan{}
	if base != nil && base.Restored != nil {
		plan = base.Restored.Plan
	}
	result := &PrismaMigrationAuthoringResult{
		State: state,
		Base:  base,
		Plan:  plan,
	}
	start := plan.PrefixLength
	if plan.SnapshotKey == "" {
		start = 0
	}
	if start < 0 {
		start = 0
	}
	if start > len(state.Migrations) {
		start = len(state.Migrations)
	}
	if start >= len(state.Migrations) {
		return result, nil
	}
	applier := opts.MigrateEach
	if applier == nil {
		applier = PrismaMigrateDeployPrefixApplier()
	}
	for i := start; i < len(state.Migrations); i++ {
		migration := state.Migrations[i]
		if err := applier(ctx, opts.DB, migration, PrismaMigrationApplyOptions{
			Worktree:      opts.Worktree,
			SchemaPath:    opts.SchemaPath,
			MigrationsDir: opts.MigrationsDir,
			Index:         i,
			LogPath:       opts.Prepare.LogPath,
			OnLine:        opts.Prepare.OnLine,
			Env:           opts.Prepare.Env,
		}); err != nil {
			return nil, err
		}
		result.Applied = true
		result.AppliedMigrations = append(result.AppliedMigrations, migration.Name)
	}
	if err := m.EnsureRuntime(ctx, opts.DB); err != nil {
		return nil, err
	}
	emitPrepareLine(opts.Prepare, "stdout", "database: waiting for managed Postgres readiness after migration replay")
	if err := m.WaitReady(ctx, opts.DB, timeout); err != nil {
		return nil, err
	}
	return result, nil
}

func (m *Manager) applyAndSnapshotEachPrismaMigration(ctx context.Context, opts PrismaDevDatabaseOptions, state *PrismaState, start int, timeout time.Duration, apply PrismaMigrationApplyFunc) (*PrismaRestoreResult, error) {
	if start < 0 {
		start = 0
	}
	if start > len(state.Migrations) {
		start = len(state.Migrations)
	}
	var snapshot *PrismaRestoreResult
	for i := start; i < len(state.Migrations); i++ {
		migration := state.Migrations[i]
		if err := apply(ctx, opts.DB, migration, PrismaMigrationApplyOptions{
			Worktree:      opts.Worktree,
			SchemaPath:    opts.SchemaPath,
			MigrationsDir: opts.MigrationsDir,
			Index:         i,
			LogPath:       opts.Prepare.LogPath,
			OnLine:        opts.Prepare.OnLine,
			Env:           opts.Prepare.Env,
		}); err != nil {
			return nil, err
		}
		prefixState := prismaStatePrefix(state, i+1)
		key := PrismaSnapshotKey(prefixState)
		item, err := m.SnapshotPrisma(ctx, opts.DB, key, prefixState)
		if err != nil {
			return nil, err
		}
		snapshot = item
		if err := m.EnsureRuntime(ctx, opts.DB); err != nil {
			return nil, err
		}
		emitPrepareLine(opts.Prepare, "stdout", "database: waiting for managed Postgres readiness after snapshot")
		if err := m.WaitReady(ctx, opts.DB, timeout); err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}

func PrismaMigrateDeployCommand(schemaPath string) process.CommandSpec {
	args := []string{"prisma", "migrate", "deploy"}
	if schemaPath != "" {
		args = append(args, "--schema", schemaPath)
	}
	return process.CommandSpec{Name: "npx", Args: args}
}

func PrismaMigrateDevCommand(schemaPath, name string, createOnly bool) process.CommandSpec {
	args := []string{"prisma", "migrate", "dev", "--name", name}
	if schemaPath != "" {
		args = append(args, "--schema", schemaPath)
	}
	if createOnly {
		args = append(args, "--create-only")
	}
	return process.CommandSpec{Name: "npx", Args: args}
}

func PrismaMigrateDeployPrefixApplier() PrismaMigrationApplyFunc {
	return func(ctx context.Context, db api.DBInstance, migration PrismaMigration, opts PrismaMigrationApplyOptions) error {
		tempRoot := filepath.Join(opts.Worktree, ".devflow", "prisma-migrate")
		if err := os.MkdirAll(tempRoot, 0o755); err != nil {
			return err
		}
		tempDir, err := os.MkdirTemp(tempRoot, "prefix-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tempDir)

		tempSchema := filepath.Join(tempDir, filepath.Base(opts.SchemaPath))
		if err := copyPath(filepath.Join(opts.Worktree, opts.SchemaPath), tempSchema); err != nil {
			return err
		}
		tempMigrations := filepath.Join(tempDir, "migrations")
		if err := os.MkdirAll(tempMigrations, 0o755); err != nil {
			return err
		}
		sourceMigrations := filepath.Join(opts.Worktree, opts.MigrationsDir)
		names, err := prismaMigrationPrefixNames(sourceMigrations, opts.Index+1)
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := copyPath(filepath.Join(sourceMigrations, name), filepath.Join(tempMigrations, name)); err != nil {
				return err
			}
		}
		return runDatabaseCommand(ctx, PrismaMigrateDeployCommand(tempSchema), opts.Worktree, PrepareOptions{
			Worktree: opts.Worktree,
			Env:      opts.Env,
			LogPath:  opts.LogPath,
			OnLine:   opts.OnLine,
		}, db)
	}
}

func GeneratePrismaMigration(ctx context.Context, opts PrismaMigrationGenerateOptions) error {
	if opts.Name == "" {
		return fmt.Errorf("migration name is required")
	}
	cmd := opts.Command
	if cmd.Name == "" {
		cmd = PrismaMigrateDevCommand(opts.SchemaPath, opts.Name, opts.CreateOnly)
	}
	if cmd.Dir == "" {
		cmd.Dir = opts.Worktree
	}
	cmd.Env = mergeStringMaps(opts.Env, cmd.Env)
	cmd.LogPath = opts.LogPath
	cmd.AppendLog = true
	cmd.OnLine = opts.OnLine
	_, err := process.Run(ctx, cmd)
	return err
}

func PrismaSnapshotKey(state *PrismaState) string {
	if state == nil || state.FullHash == "" {
		return "prisma_unknown"
	}
	return SnapshotKey("prisma", shortHash(state.FullHash))
}

func prismaStatePrefix(state *PrismaState, prefix int) *PrismaState {
	if state == nil {
		return nil
	}
	if prefix < 0 {
		prefix = 0
	}
	if prefix > len(state.Migrations) {
		prefix = len(state.Migrations)
	}
	out := &PrismaState{
		SchemaHash:      state.SchemaHash,
		BaseFingerprint: state.BaseFingerprint,
		PathHashes:      clonePathHashes(state.PathHashes),
		Migrations:      append([]PrismaMigration(nil), state.Migrations[:prefix]...),
	}
	parts := []string{"schema:" + out.SchemaHash, "base:" + out.BaseFingerprint}
	for _, migration := range out.Migrations {
		parts = append(parts, migration.Name+":"+migration.Hash)
	}
	out.FullHash = hashStrings(parts)
	return out
}

func prismaMigrationPrefixNames(root string, prefix int) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	sortStrings(names)
	if prefix < 0 {
		prefix = 0
	}
	if prefix > len(names) {
		prefix = len(names)
	}
	return names[:prefix], nil
}

func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyDir(src, dst, info.Mode())
	}
	return copyFile(src, dst, info.Mode())
}

func copyDir(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(dst, mode); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyPath(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}
