package database

import (
	"context"

	"github.com/benjaco/devflow/pkg/project"
)

type PrismaDevDatabaseSummary struct {
	Applied              bool     `json:"applied"`
	SnapshotKey          string   `json:"snapshotKey,omitempty"`
	PlanSnapshot         string   `json:"planSnapshot,omitempty"`
	ExactMatch           bool     `json:"exactMatch"`
	PrefixLength         int      `json:"prefixLength"`
	MigrationCount       int      `json:"migrationCount"`
	MigrationNames       []string `json:"migrationNames,omitempty"`
	Restored             bool     `json:"restored"`
	RestoredSnapshot     string   `json:"restoredSnapshot,omitempty"`
	RestoredExactMatch   bool     `json:"restoredExactMatch"`
	RestoredPrefixLength int      `json:"restoredPrefixLength"`
	Recreated            bool     `json:"recreated"`
	SourceApplied        bool     `json:"sourceApplied"`
	SourcePolicy         string   `json:"sourcePolicy,omitempty"`
}

type PrismaMigrationAuthoringSummary struct {
	Applied              bool     `json:"applied"`
	AppliedMigrations    []string `json:"appliedMigrations,omitempty"`
	PlanSnapshot         string   `json:"planSnapshot,omitempty"`
	ExactMatch           bool     `json:"exactMatch"`
	PrefixLength         int      `json:"prefixLength"`
	MigrationCount       int      `json:"migrationCount"`
	MigrationNames       []string `json:"migrationNames,omitempty"`
	Restored             bool     `json:"restored"`
	RestoredSnapshot     string   `json:"restoredSnapshot,omitempty"`
	RestoredExactMatch   bool     `json:"restoredExactMatch"`
	RestoredPrefixLength int      `json:"restoredPrefixLength"`
	Recreated            bool     `json:"recreated"`
	SourceApplied        bool     `json:"sourceApplied"`
	SourcePolicy         string   `json:"sourcePolicy,omitempty"`
}

func PrepareOptionsFromRuntime(rt *project.Runtime) PrepareOptions {
	if rt == nil {
		return PrepareOptions{}
	}
	return PrepareOptions{
		Worktree: rt.Worktree,
		Env:      rt.CloneEnv(),
		LogPath:  rt.LogPath,
		OnLine:   rt.EventLineEmitter(),
	}
}

func EnsurePrismaDevDatabaseForRuntime(ctx context.Context, rt *project.Runtime, opts PrismaDevDatabaseOptions) (*PrismaDevDatabaseResult, error) {
	if rt != nil {
		if opts.Worktree == "" {
			opts.Worktree = rt.Worktree
		}
		if opts.DB.Name == "" && rt.Instance != nil {
			opts.DB = rt.Instance.DB
		}
		if opts.Prepare.Worktree == "" && opts.Prepare.LogPath == "" && opts.Prepare.OnLine == nil {
			opts.Prepare = PrepareOptionsFromRuntime(rt)
		}
	}
	return New().EnsurePrismaDevDatabase(ctx, opts)
}

func PreparePrismaMigrationAuthoringDatabaseForRuntime(ctx context.Context, rt *project.Runtime, opts PrismaMigrationAuthoringOptions) (*PrismaMigrationAuthoringResult, error) {
	if rt != nil {
		if opts.Worktree == "" {
			opts.Worktree = rt.Worktree
		}
		if opts.DB.Name == "" && rt.Instance != nil {
			opts.DB = rt.Instance.DB
		}
		if opts.Prepare.Worktree == "" && opts.Prepare.LogPath == "" && opts.Prepare.OnLine == nil {
			opts.Prepare = PrepareOptionsFromRuntime(rt)
		}
	}
	return New().PreparePrismaMigrationAuthoringDatabase(ctx, opts)
}

func GeneratePrismaMigrationForRuntime(ctx context.Context, rt *project.Runtime, opts PrismaMigrationGenerateOptions) error {
	if rt != nil {
		if opts.Worktree == "" {
			opts.Worktree = rt.Worktree
		}
		if opts.Env == nil {
			opts.Env = rt.CloneEnv()
		}
		if opts.LogPath == "" {
			opts.LogPath = rt.LogPath
		}
		if opts.OnLine == nil {
			opts.OnLine = rt.EventLineEmitter()
		}
	}
	return GeneratePrismaMigration(ctx, opts)
}

func SummarizePrismaDevDatabase(result *PrismaDevDatabaseResult) PrismaDevDatabaseSummary {
	if result == nil {
		return PrismaDevDatabaseSummary{}
	}
	summary := PrismaDevDatabaseSummary{
		Applied:      result.Applied,
		SnapshotKey:  result.SnapshotKey,
		PlanSnapshot: result.Plan.SnapshotKey,
		ExactMatch:   result.Plan.ExactMatch,
		PrefixLength: result.Plan.PrefixLength,
	}
	if result.State != nil {
		summary.MigrationCount = len(result.State.Migrations)
		for _, migration := range result.State.Migrations {
			summary.MigrationNames = append(summary.MigrationNames, migration.Name)
		}
	}
	if result.Base != nil {
		summary.Restored = result.Base.Restored != nil
		if result.Base.Restored != nil {
			summary.RestoredSnapshot = result.Base.Restored.Plan.SnapshotKey
			summary.RestoredExactMatch = result.Base.Restored.Plan.ExactMatch
			summary.RestoredPrefixLength = result.Base.Restored.Plan.PrefixLength
		}
		summary.Recreated = result.Base.Recreated
		summary.SourceApplied = result.Base.SourceApplied
		summary.SourcePolicy = result.Base.SourcePolicy
	}
	return summary
}

func SummarizePrismaMigrationAuthoring(result *PrismaMigrationAuthoringResult) PrismaMigrationAuthoringSummary {
	if result == nil {
		return PrismaMigrationAuthoringSummary{}
	}
	summary := PrismaMigrationAuthoringSummary{
		Applied:           result.Applied,
		AppliedMigrations: append([]string(nil), result.AppliedMigrations...),
		PlanSnapshot:      result.Plan.SnapshotKey,
		ExactMatch:        result.Plan.ExactMatch,
		PrefixLength:      result.Plan.PrefixLength,
	}
	if result.State != nil {
		summary.MigrationCount = len(result.State.Migrations)
		for _, migration := range result.State.Migrations {
			summary.MigrationNames = append(summary.MigrationNames, migration.Name)
		}
	}
	if result.Base != nil {
		summary.Restored = result.Base.Restored != nil
		if result.Base.Restored != nil {
			summary.RestoredSnapshot = result.Base.Restored.Plan.SnapshotKey
			summary.RestoredExactMatch = result.Base.Restored.Plan.ExactMatch
			summary.RestoredPrefixLength = result.Base.Restored.Plan.PrefixLength
		}
		summary.Recreated = result.Base.Recreated
		summary.SourceApplied = result.Base.SourceApplied
		summary.SourcePolicy = result.Base.SourcePolicy
	}
	return summary
}
