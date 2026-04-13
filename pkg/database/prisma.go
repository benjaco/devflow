package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"devflow/internal/jsonutil"
	"devflow/pkg/api"
)

type PrismaMigration struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

type PrismaState struct {
	SchemaHash      string            `json:"schemaHash"`
	BaseFingerprint string            `json:"baseFingerprint,omitempty"`
	PathHashes      map[string]string `json:"pathHashes,omitempty"`
	Migrations      []PrismaMigration `json:"migrations"`
	FullHash        string            `json:"fullHash"`
}

type PrismaSnapshot struct {
	Version         int               `json:"version"`
	Key             string            `json:"key"`
	CreatedAt       time.Time         `json:"createdAt"`
	SchemaHash      string            `json:"schemaHash"`
	BaseFingerprint string            `json:"baseFingerprint,omitempty"`
	PathHashes      map[string]string `json:"pathHashes,omitempty"`
	Migrations      []PrismaMigration `json:"migrations"`
	FullHash        string            `json:"fullHash"`
}

type PrismaRestorePlan struct {
	ExactMatch   bool            `json:"exactMatch"`
	SnapshotKey  string          `json:"snapshotKey,omitempty"`
	PrefixLength int             `json:"prefixLength"`
	Snapshot     *PrismaSnapshot `json:"snapshot,omitempty"`
}

type PrismaRestoreResult struct {
	Manifest *SnapshotManifest `json:"manifest,omitempty"`
	Metadata *PrismaSnapshot   `json:"metadata,omitempty"`
	Plan     PrismaRestorePlan `json:"plan"`
}

func InspectPrismaState(worktree, schemaPath, migrationsDir string, extraPaths []string) (*PrismaState, error) {
	schemaHash, err := hashPath(filepath.Join(worktree, schemaPath))
	if err != nil {
		return nil, err
	}

	migrations, err := collectPrismaMigrations(filepath.Join(worktree, migrationsDir))
	if err != nil {
		return nil, err
	}

	pathHashes := map[string]string{}
	baseParts := make([]string, 0, len(extraPaths))
	for _, rel := range extraPaths {
		abs := filepath.Join(worktree, rel)
		sum, err := hashPath(abs)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		pathHashes[rel] = sum
		baseParts = append(baseParts, rel+":"+sum)
	}
	sort.Strings(baseParts)
	baseFingerprint := hashStrings(baseParts)

	fullParts := []string{"schema:" + schemaHash, "base:" + baseFingerprint}
	for _, migration := range migrations {
		fullParts = append(fullParts, migration.Name+":"+migration.Hash)
	}

	return &PrismaState{
		SchemaHash:      schemaHash,
		BaseFingerprint: baseFingerprint,
		PathHashes:      pathHashes,
		Migrations:      migrations,
		FullHash:        hashStrings(fullParts),
	}, nil
}

func SavePrismaSnapshot(root, key string, state *PrismaState) (*PrismaSnapshot, error) {
	snapshotDir := filepath.Join(root, key)
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		return nil, err
	}
	meta := &PrismaSnapshot{
		Version:         1,
		Key:             key,
		CreatedAt:       time.Now().UTC(),
		SchemaHash:      state.SchemaHash,
		BaseFingerprint: state.BaseFingerprint,
		PathHashes:      clonePathHashes(state.PathHashes),
		Migrations:      append([]PrismaMigration(nil), state.Migrations...),
		FullHash:        state.FullHash,
	}
	if err := jsonutil.WriteFileAtomic(filepath.Join(snapshotDir, "prisma.json"), meta); err != nil {
		return nil, err
	}
	return meta, nil
}

func LoadPrismaSnapshot(root, key string) (*PrismaSnapshot, error) {
	var meta PrismaSnapshot
	if err := jsonutil.ReadFile(filepath.Join(root, key, "prisma.json"), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func PlanPrismaRestore(root string, state *PrismaState) (PrismaRestorePlan, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return PrismaRestorePlan{}, nil
		}
		return PrismaRestorePlan{}, err
	}

	best := PrismaRestorePlan{}
	bestPrefix := -1
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, err := LoadPrismaSnapshot(root, entry.Name())
		if err != nil {
			continue
		}
		if meta.FullHash == state.FullHash {
			return PrismaRestorePlan{
				ExactMatch:   true,
				SnapshotKey:  entry.Name(),
				PrefixLength: len(meta.Migrations),
				Snapshot:     meta,
			}, nil
		}
		if meta.BaseFingerprint != state.BaseFingerprint {
			continue
		}
		prefixLen := migrationPrefix(meta.Migrations, state.Migrations)
		if prefixLen == len(meta.Migrations) && prefixLen > bestPrefix {
			bestPrefix = prefixLen
			best = PrismaRestorePlan{
				ExactMatch:   false,
				SnapshotKey:  entry.Name(),
				PrefixLength: prefixLen,
				Snapshot:     meta,
			}
		}
	}
	if bestPrefix < 0 {
		return PrismaRestorePlan{}, nil
	}
	return best, nil
}

func (m *Manager) SnapshotPrisma(ctx context.Context, db api.DBInstance, key string, state *PrismaState) (*PrismaRestoreResult, error) {
	manifest, err := m.Snapshot(ctx, db, key)
	if err != nil {
		return nil, err
	}
	meta, err := SavePrismaSnapshot(db.SnapshotRoot, key, state)
	if err != nil {
		return nil, err
	}
	return &PrismaRestoreResult{
		Manifest: manifest,
		Metadata: meta,
		Plan: PrismaRestorePlan{
			ExactMatch:   true,
			SnapshotKey:  key,
			PrefixLength: len(state.Migrations),
			Snapshot:     meta,
		},
	}, nil
}

func (m *Manager) RestoreNearestPrismaSnapshot(ctx context.Context, db api.DBInstance, state *PrismaState) (*PrismaRestoreResult, error) {
	plan, err := PlanPrismaRestore(db.SnapshotRoot, state)
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
	return &PrismaRestoreResult{
		Manifest: manifest,
		Metadata: plan.Snapshot,
		Plan:     plan,
	}, nil
}

func collectPrismaMigrations(root string) ([]PrismaMigration, error) {
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
	sort.Strings(names)
	out := make([]PrismaMigration, 0, len(names))
	for _, name := range names {
		out = append(out, PrismaMigration{Name: name, Hash: items[name]})
	}
	return out, nil
}

func migrationPrefix(candidate, current []PrismaMigration) int {
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

func hashPath(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return hashDir(path)
	}
	return hashFile(path)
}

func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func hashDir(root string) (string, error) {
	parts := make([]string, 0)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			parts = append(parts, "dir:"+rel)
			return nil
		}
		sum, err := hashFile(path)
		if err != nil {
			return err
		}
		parts = append(parts, "file:"+rel+":"+sum)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(parts)
	return hashStrings(parts), nil
}

func hashStrings(parts []string) string {
	h := sha256.New()
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func clonePathHashes(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
