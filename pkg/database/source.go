package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/process"
)

type PrepareOptions struct {
	Worktree string
	Env      map[string]string
	LogPath  string
	OnLine   func(stream, line string)
}

type SourcePolicy interface {
	Name() string
	PrepareBase(ctx context.Context, db api.DBInstance, opts PrepareOptions) error
}

type SourcePolicyFunc struct {
	PolicyName string
	Fn         func(ctx context.Context, db api.DBInstance, opts PrepareOptions) error
}

func (p SourcePolicyFunc) Name() string {
	return p.PolicyName
}

func (p SourcePolicyFunc) PrepareBase(ctx context.Context, db api.DBInstance, opts PrepareOptions) error {
	if p.Fn == nil {
		return nil
	}
	return p.Fn(ctx, db, opts)
}

type CommandSourcePolicy struct {
	PolicyName string
	Spec       process.CommandSpec
}

func (p CommandSourcePolicy) Name() string {
	return p.PolicyName
}

func (p CommandSourcePolicy) PrepareBase(ctx context.Context, db api.DBInstance, opts PrepareOptions) error {
	spec := p.Spec
	if spec.Dir == "" {
		spec.Dir = opts.Worktree
	}
	spec.LogPath = opts.LogPath
	spec.AppendLog = true
	spec.OnLine = opts.OnLine
	spec.Env = mergeStringMaps(opts.Env, databaseEnv(db))
	_, err := process.Run(ctx, spec)
	return err
}

type PostgresDumpSourcePolicy struct {
	PolicyName string
	RemoteURL  string
}

func (p PostgresDumpSourcePolicy) Name() string {
	if p.PolicyName != "" {
		return p.PolicyName
	}
	return "postgres-dump"
}

func (p PostgresDumpSourcePolicy) PrepareBase(ctx context.Context, db api.DBInstance, opts PrepareOptions) error {
	if p.RemoteURL == "" {
		return fmt.Errorf("remote database URL is required")
	}
	dump, err := os.CreateTemp("", "devflow-pg-dump-*.sql")
	if err != nil {
		return fmt.Errorf("create temporary Postgres dump: %w", err)
	}
	dumpPath := dump.Name()
	if err := dump.Close(); err != nil {
		_ = os.Remove(dumpPath)
		return fmt.Errorf("close temporary Postgres dump: %w", err)
	}
	defer os.Remove(dumpPath)

	env := mergeStringMaps(opts.Env, mergeStringMaps(databaseEnv(db), map[string]string{
		"DEVFLOW_REMOTE_DATABASE_URL": p.RemoteURL,
	}))
	common := process.CommandSpec{
		Dir:       opts.Worktree,
		Env:       env,
		LogPath:   opts.LogPath,
		AppendLog: true,
		OnLine:    opts.OnLine,
	}
	dumpSpec := common
	dumpSpec.Name = "pg_dump"
	dumpSpec.Args = []string{"--no-owner", "--no-privileges", "-f", dumpPath, p.RemoteURL}
	if _, err := process.Run(ctx, dumpSpec); err != nil {
		return fmt.Errorf("dump remote Postgres database: %w", err)
	}
	restoreSpec := common
	restoreSpec.Name = "psql"
	restoreSpec.Args = []string{db.URL, "-v", "ON_ERROR_STOP=1", "-f", dumpPath}
	if _, err := process.Run(ctx, restoreSpec); err != nil {
		return fmt.Errorf("restore remote Postgres database: %w", err)
	}
	return nil
}

type PrismaBaseResult struct {
	Restored      *PrismaRestoreResult `json:"restored,omitempty"`
	Recreated     bool                 `json:"recreated"`
	SourceApplied bool                 `json:"sourceApplied"`
	SourcePolicy  string               `json:"sourcePolicy,omitempty"`
}

func (m *Manager) PreparePrismaBase(ctx context.Context, db api.DBInstance, state *PrismaState, policy SourcePolicy, opts PrepareOptions) (*PrismaBaseResult, error) {
	emitPrepareLine(opts, "stdout", "database: checking cached Prisma migration snapshots")
	restored, err := m.RestoreNearestPrismaSnapshot(ctx, db, state)
	if err != nil {
		return nil, err
	}
	if restored != nil {
		emitPrepareLine(opts, "stdout", fmt.Sprintf("database: restored Prisma snapshot %s", restored.Plan.SnapshotKey))
		return &PrismaBaseResult{Restored: restored}, nil
	}
	emitPrepareLine(opts, "stdout", "database: no compatible Prisma snapshot; recreating managed Postgres volume")
	if err := m.DestroyRuntime(ctx, db, true); err != nil {
		return nil, err
	}
	result := &PrismaBaseResult{Recreated: true}
	if policy == nil {
		return result, nil
	}
	emitPrepareLine(opts, "stdout", "database: starting managed Postgres runtime for source clone")
	if err := m.EnsureRuntime(ctx, db); err != nil {
		return nil, err
	}
	emitPrepareLine(opts, "stdout", "database: waiting for managed Postgres readiness")
	if err := m.WaitReady(ctx, db, 30*time.Second); err != nil {
		return nil, err
	}
	emitPrepareLine(opts, "stdout", fmt.Sprintf("database: applying source policy %s", policy.Name()))
	if err := policy.PrepareBase(ctx, db, opts); err != nil {
		return nil, err
	}
	emitPrepareLine(opts, "stdout", "database: stopping managed Postgres after source clone")
	if err := m.StopRuntime(ctx, db); err != nil {
		return nil, err
	}
	result.SourceApplied = true
	result.SourcePolicy = policy.Name()
	return result, nil
}

func databaseEnv(db api.DBInstance) map[string]string {
	return map[string]string{
		"DATABASE_URL": db.URL,
		"PGHOST":       db.Host,
		"PGPORT":       strconv.Itoa(db.Port),
		"PGDATABASE":   db.Name,
		"PGUSER":       db.User,
		"PGPASSWORD":   db.Password,
	}
}

func mergeStringMaps(base, overlay map[string]string) map[string]string {
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

func emitPrepareLine(opts PrepareOptions, stream, line string) {
	if opts.LogPath != "" {
		_ = os.MkdirAll(filepath.Dir(opts.LogPath), 0o755)
		file, err := os.OpenFile(opts.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(file, "%s: %s\n", stream, line)
			_ = file.Close()
		}
	}
	if opts.OnLine != nil {
		opts.OnLine(stream, line)
	}
}
