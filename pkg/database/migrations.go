package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/benjaco/devflow/internal/jsonutil"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/process"
)

type MigrationPoint struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

type MigrationState struct {
	BaseFingerprint string            `json:"baseFingerprint,omitempty"`
	PathHashes      map[string]string `json:"pathHashes,omitempty"`
	Migrations      []MigrationPoint  `json:"migrations"`
	FullHash        string            `json:"fullHash"`
}

type MigrationSnapshot struct {
	Version         int               `json:"version"`
	Key             string            `json:"key"`
	CreatedAt       time.Time         `json:"createdAt"`
	BaseFingerprint string            `json:"baseFingerprint,omitempty"`
	PathHashes      map[string]string `json:"pathHashes,omitempty"`
	Migrations      []MigrationPoint  `json:"migrations"`
	FullHash        string            `json:"fullHash"`
}

type MigrationRestorePlan struct {
	ExactMatch   bool               `json:"exactMatch"`
	SnapshotKey  string             `json:"snapshotKey,omitempty"`
	PrefixLength int                `json:"prefixLength"`
	Snapshot     *MigrationSnapshot `json:"snapshot,omitempty"`
}

type MigrationRestoreResult struct {
	Manifest *SnapshotManifest    `json:"manifest,omitempty"`
	Metadata *MigrationSnapshot   `json:"metadata,omitempty"`
	Plan     MigrationRestorePlan `json:"plan"`
}

type MigrationBaseResult struct {
	Restored      *MigrationRestoreResult `json:"restored,omitempty"`
	Recreated     bool                    `json:"recreated"`
	SourceApplied bool                    `json:"sourceApplied"`
	SourcePolicy  string                  `json:"sourcePolicy,omitempty"`
}

type ManagedMigrationOptions struct {
	Worktree      string
	DB            api.DBInstance
	MigrationsDir string
	BasePaths     []string
	SourcePolicy  SourcePolicy
	Prepare       PrepareOptions
	Apply         process.CommandSpec
	ApplyEach     MigrationApplyFunc
	SnapshotKey   string
	ReadyTimeout  time.Duration
}

type MigrationApplyFunc func(ctx context.Context, db api.DBInstance, migration MigrationPoint, opts MigrationApplyOptions) error

type MigrationApplyOptions struct {
	Worktree      string
	MigrationsDir string
	Index         int
	LogPath       string
	OnLine        func(stream, line string)
	Env           map[string]string
}

type ManagedMigrationResult struct {
	State       *MigrationState         `json:"state,omitempty"`
	Base        *MigrationBaseResult    `json:"base,omitempty"`
	Applied     bool                    `json:"applied"`
	Snapshot    *MigrationRestoreResult `json:"snapshot,omitempty"`
	SnapshotKey string                  `json:"snapshotKey,omitempty"`
	Plan        MigrationRestorePlan    `json:"plan"`
}

func InspectMigrationState(worktree, migrationsDir string, basePaths []string) (*MigrationState, error) {
	migrations, err := collectMigrationPoints(filepath.Join(worktree, migrationsDir))
	if err != nil {
		return nil, err
	}
	pathHashes := map[string]string{}
	baseParts := make([]string, 0, len(basePaths))
	for _, rel := range basePaths {
		abs := filepath.Join(worktree, rel)
		sum, err := hashPath(abs)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		pathHashes[rel] = sum
		baseParts = append(baseParts, rel+":"+sum)
	}
	baseFingerprint := hashStrings(baseParts)
	fullParts := []string{"base:" + baseFingerprint}
	for _, migration := range migrations {
		fullParts = append(fullParts, migration.Name+":"+migration.Hash)
	}
	return &MigrationState{
		BaseFingerprint: baseFingerprint,
		PathHashes:      pathHashes,
		Migrations:      migrations,
		FullHash:        hashStrings(fullParts),
	}, nil
}

func SaveMigrationSnapshot(root, key string, state *MigrationState) (*MigrationSnapshot, error) {
	snapshotDir := filepath.Join(root, key)
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		return nil, err
	}
	meta := &MigrationSnapshot{
		Version:         1,
		Key:             key,
		CreatedAt:       time.Now().UTC(),
		BaseFingerprint: state.BaseFingerprint,
		PathHashes:      clonePathHashes(state.PathHashes),
		Migrations:      append([]MigrationPoint(nil), state.Migrations...),
		FullHash:        state.FullHash,
	}
	if err := jsonutil.WriteFileAtomic(filepath.Join(snapshotDir, "migrations.json"), meta); err != nil {
		return nil, err
	}
	return meta, nil
}

func LoadMigrationSnapshot(root, key string) (*MigrationSnapshot, error) {
	var meta MigrationSnapshot
	if err := jsonutil.ReadFile(filepath.Join(root, key, "migrations.json"), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func PlanMigrationRestore(root string, state *MigrationState) (MigrationRestorePlan, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return MigrationRestorePlan{}, nil
		}
		return MigrationRestorePlan{}, err
	}
	best := MigrationRestorePlan{}
	bestPrefix := -1
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, err := LoadMigrationSnapshot(root, entry.Name())
		if err != nil {
			continue
		}
		if meta.FullHash == state.FullHash {
			return MigrationRestorePlan{
				ExactMatch:   true,
				SnapshotKey:  entry.Name(),
				PrefixLength: len(meta.Migrations),
				Snapshot:     meta,
			}, nil
		}
		if meta.BaseFingerprint != state.BaseFingerprint {
			continue
		}
		prefixLen := migrationPointPrefix(meta.Migrations, state.Migrations)
		if prefixLen == len(meta.Migrations) && prefixLen > bestPrefix {
			bestPrefix = prefixLen
			best = MigrationRestorePlan{
				ExactMatch:   false,
				SnapshotKey:  entry.Name(),
				PrefixLength: prefixLen,
				Snapshot:     meta,
			}
		}
	}
	if bestPrefix < 0 {
		return MigrationRestorePlan{}, nil
	}
	return best, nil
}

func (m *Manager) SnapshotMigrations(ctx context.Context, db api.DBInstance, key string, state *MigrationState) (*MigrationRestoreResult, error) {
	manifest, err := m.Snapshot(ctx, db, key)
	if err != nil {
		return nil, err
	}
	meta, err := SaveMigrationSnapshot(db.SnapshotRoot, key, state)
	if err != nil {
		return nil, err
	}
	return &MigrationRestoreResult{
		Manifest: manifest,
		Metadata: meta,
		Plan: MigrationRestorePlan{
			ExactMatch:   true,
			SnapshotKey:  key,
			PrefixLength: len(state.Migrations),
			Snapshot:     meta,
		},
	}, nil
}

func (m *Manager) RestoreNearestMigrationSnapshot(ctx context.Context, db api.DBInstance, state *MigrationState) (*MigrationRestoreResult, error) {
	plan, err := PlanMigrationRestore(db.SnapshotRoot, state)
	if err != nil {
		return nil, err
	}
	if plan.SnapshotKey == "" {
		return nil, nil
	}
	manifest, err := m.RestoreSnapshot(ctx, db, plan.SnapshotKey)
	if err != nil {
		return nil, err
	}
	return &MigrationRestoreResult{
		Manifest: manifest,
		Metadata: plan.Snapshot,
		Plan:     plan,
	}, nil
}

func (m *Manager) PrepareMigrationBase(ctx context.Context, db api.DBInstance, state *MigrationState, policy SourcePolicy, opts PrepareOptions) (*MigrationBaseResult, error) {
	restored, err := m.RestoreNearestMigrationSnapshot(ctx, db, state)
	if err != nil {
		return nil, err
	}
	if restored != nil {
		return &MigrationBaseResult{Restored: restored}, nil
	}
	if err := m.DestroyRuntime(ctx, db, true); err != nil {
		return nil, err
	}
	result := &MigrationBaseResult{Recreated: true}
	if policy == nil {
		return result, nil
	}
	if err := m.EnsureRuntime(ctx, db); err != nil {
		return nil, err
	}
	if err := m.WaitReady(ctx, db, 30*time.Second); err != nil {
		return nil, err
	}
	if err := policy.PrepareBase(ctx, db, opts); err != nil {
		return nil, err
	}
	if err := m.StopRuntime(ctx, db); err != nil {
		return nil, err
	}
	result.SourceApplied = true
	result.SourcePolicy = policy.Name()
	return result, nil
}

func (m *Manager) EnsureMigratedDatabase(ctx context.Context, opts ManagedMigrationOptions) (*ManagedMigrationResult, error) {
	state, err := InspectMigrationState(opts.Worktree, opts.MigrationsDir, opts.BasePaths)
	if err != nil {
		return nil, err
	}
	base, err := m.PrepareMigrationBase(ctx, opts.DB, state, opts.SourcePolicy, opts.Prepare)
	if err != nil {
		return nil, err
	}
	if err := m.EnsureRuntime(ctx, opts.DB); err != nil {
		return nil, err
	}
	timeout := opts.ReadyTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if err := m.WaitReady(ctx, opts.DB, timeout); err != nil {
		return nil, err
	}
	plan := MigrationRestorePlan{}
	if base != nil && base.Restored != nil {
		plan = base.Restored.Plan
	}
	result := &ManagedMigrationResult{State: state, Base: base, Plan: plan}
	needsApply := plan.SnapshotKey == "" || !plan.ExactMatch
	if needsApply && len(state.Migrations) > 0 {
		if opts.ApplyEach != nil {
			snapshot, err := m.applyAndSnapshotEachMigration(ctx, opts, state, plan.PrefixLength, timeout)
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
		if opts.Apply.Name == "" {
			return nil, fmt.Errorf("migration apply command is required when database snapshot is not exact")
		}
		if err := runDatabaseCommand(ctx, opts.Apply, opts.Worktree, opts.Prepare, opts.DB); err != nil {
			return nil, err
		}
		result.Applied = true
	}
	if needsApply {
		key := opts.SnapshotKey
		if key == "" {
			key = MigrationSnapshotKey(state)
		}
		snapshot, err := m.SnapshotMigrations(ctx, opts.DB, key, state)
		if err != nil {
			return nil, err
		}
		result.Snapshot = snapshot
		result.SnapshotKey = key
		result.Plan = snapshot.Plan
		if err := m.EnsureRuntime(ctx, opts.DB); err != nil {
			return nil, err
		}
		if err := m.WaitReady(ctx, opts.DB, timeout); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (m *Manager) applyAndSnapshotEachMigration(ctx context.Context, opts ManagedMigrationOptions, state *MigrationState, start int, timeout time.Duration) (*MigrationRestoreResult, error) {
	if start < 0 {
		start = 0
	}
	if start > len(state.Migrations) {
		start = len(state.Migrations)
	}
	var snapshot *MigrationRestoreResult
	for i := start; i < len(state.Migrations); i++ {
		migration := state.Migrations[i]
		if err := opts.ApplyEach(ctx, opts.DB, migration, MigrationApplyOptions{
			Worktree:      opts.Worktree,
			MigrationsDir: opts.MigrationsDir,
			Index:         i,
			LogPath:       opts.Prepare.LogPath,
			OnLine:        opts.Prepare.OnLine,
			Env:           opts.Prepare.Env,
		}); err != nil {
			return nil, err
		}
		prefixState := migrationStatePrefix(state, i+1)
		key := MigrationSnapshotKey(prefixState)
		item, err := m.SnapshotMigrations(ctx, opts.DB, key, prefixState)
		if err != nil {
			return nil, err
		}
		snapshot = item
		if err := m.EnsureRuntime(ctx, opts.DB); err != nil {
			return nil, err
		}
		if err := m.WaitReady(ctx, opts.DB, timeout); err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}

func MigrationSnapshotKey(state *MigrationState) string {
	if state == nil || state.FullHash == "" {
		return "migrations_unknown"
	}
	return SnapshotKey("migrations", shortHash(state.FullHash))
}

func PostgresMigrationFileApplier(fileName string) MigrationApplyFunc {
	return func(ctx context.Context, db api.DBInstance, migration MigrationPoint, opts MigrationApplyOptions) error {
		migrationPath := filepath.Join(opts.Worktree, opts.MigrationsDir, migration.Name)
		if fileName != "" {
			migrationPath = filepath.Join(migrationPath, fileName)
		}
		spec := process.CommandSpec{
			Name: "sh",
			Args: []string{"-c", `psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$DEVFLOW_MIGRATION_PATH"`},
			Dir:  opts.Worktree,
			Env: mergeStringMaps(opts.Env, mergeStringMaps(databaseEnv(db), map[string]string{
				"DEVFLOW_MIGRATION_NAME":  migration.Name,
				"DEVFLOW_MIGRATION_PATH":  migrationPath,
				"DEVFLOW_MIGRATION_INDEX": fmt.Sprintf("%d", opts.Index),
			})),
			LogPath: opts.LogPath,
			OnLine:  opts.OnLine,
		}
		_, err := process.Run(ctx, spec)
		return err
	}
}

func migrationStatePrefix(state *MigrationState, prefix int) *MigrationState {
	if state == nil {
		return nil
	}
	if prefix < 0 {
		prefix = 0
	}
	if prefix > len(state.Migrations) {
		prefix = len(state.Migrations)
	}
	out := &MigrationState{
		BaseFingerprint: state.BaseFingerprint,
		PathHashes:      clonePathHashes(state.PathHashes),
		Migrations:      append([]MigrationPoint(nil), state.Migrations[:prefix]...),
	}
	parts := []string{"base:" + out.BaseFingerprint}
	for _, migration := range out.Migrations {
		parts = append(parts, migration.Name+":"+migration.Hash)
	}
	out.FullHash = hashStrings(parts)
	return out
}

func collectMigrationPoints(root string) ([]MigrationPoint, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	items := make(map[string]string, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		sum, err := hashPath(filepath.Join(root, name))
		if err != nil {
			return nil, err
		}
		names = append(names, name)
		items[name] = sum
	}
	sortStrings(names)
	out := make([]MigrationPoint, 0, len(names))
	for _, name := range names {
		out = append(out, MigrationPoint{Name: name, Hash: items[name]})
	}
	return out, nil
}

func migrationPointPrefix(candidate, current []MigrationPoint) int {
	if len(candidate) > len(current) {
		return -1
	}
	for i := range candidate {
		if candidate[i] != current[i] {
			return -1
		}
	}
	return len(candidate)
}

func runDatabaseCommand(ctx context.Context, spec process.CommandSpec, worktree string, opts PrepareOptions, db api.DBInstance) error {
	if spec.Dir == "" {
		spec.Dir = worktree
	}
	spec.LogPath = opts.LogPath
	spec.OnLine = opts.OnLine
	spec.Env = mergeStringMaps(opts.Env, mergeStringMaps(databaseEnv(db), spec.Env))
	_, err := process.Run(ctx, spec)
	return err
}

func shortHash(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func sortStrings(items []string) {
	sort.Strings(items)
}
