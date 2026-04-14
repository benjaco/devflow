package database

import (
	"context"
	"strconv"
	"time"

	"devflow/pkg/api"
	"devflow/pkg/process"
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
	spec.OnLine = opts.OnLine
	spec.Env = mergeStringMaps(opts.Env, databaseEnv(db))
	_, err := process.Run(ctx, spec)
	return err
}

type PrismaBaseResult struct {
	Restored      *PrismaRestoreResult `json:"restored,omitempty"`
	Recreated     bool                 `json:"recreated"`
	SourceApplied bool                 `json:"sourceApplied"`
	SourcePolicy  string               `json:"sourcePolicy,omitempty"`
}

func (m *Manager) PreparePrismaBase(ctx context.Context, db api.DBInstance, state *PrismaState, policy SourcePolicy, opts PrepareOptions) (*PrismaBaseResult, error) {
	restored, err := m.RestoreNearestPrismaSnapshot(ctx, db, state)
	if err != nil {
		return nil, err
	}
	if restored != nil {
		return &PrismaBaseResult{Restored: restored}, nil
	}
	if err := m.DestroyRuntime(ctx, db, true); err != nil {
		return nil, err
	}
	result := &PrismaBaseResult{Recreated: true}
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
