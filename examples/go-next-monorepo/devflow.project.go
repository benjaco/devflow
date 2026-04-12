package gonextmonorepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"devflow/internal/fsutil"
	"devflow/pkg/api"
	"devflow/pkg/instance"
	"devflow/pkg/process"
	"devflow/pkg/project"
)

type exampleProject struct{}

func init() {
	project.Register(exampleProject{})
}

func (exampleProject) Name() string {
	return "go-next-monorepo"
}

func (exampleProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	id, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		return project.InstanceConfig{}, err
	}
	return project.InstanceConfig{
		Label:     filepath.Base(worktree),
		PortNames: []string{"backend", "frontend"},
		Env: map[string]string{
			"DEVFLOW_EXAMPLE_PROJECT": "go-next-monorepo",
			"PGDATABASE":              fmt.Sprintf("app_wt_%s", id),
			"DATABASE_URL":            fmt.Sprintf("postgres://devflow@localhost/app_wt_%s", id),
		},
		DB: api.DBInstance{
			Name: fmt.Sprintf("app_wt_%s", id),
			URL:  fmt.Sprintf("postgres://devflow@localhost/app_wt_%s", id),
		},
	}, nil
}

func (exampleProject) Tasks() []project.Task {
	return []project.Task{
		project.ShellTask(
			"warmup_node_install",
			"Warm a local node install cache",
			project.KindWarmup,
			nil,
			false,
			project.Outputs{},
			project.Inputs{Files: []string{"frontend/package-lock.json"}},
			"mkdir -p .devflow/example/warmup && printf 'node install warmup\\n' > .devflow/example/warmup/node-install.txt",
		),
		project.ShellTask(
			"warmup_pull_postgres_image",
			"Warm postgres image metadata",
			project.KindWarmup,
			nil,
			false,
			project.Outputs{},
			project.Inputs{Files: []string{"tools/postgres-image.txt"}},
			"mkdir -p .devflow/example/warmup && printf 'postgres image warmup\\n' > .devflow/example/warmup/postgres-image.txt",
		),
		{
			Name:        "ensure_db",
			Kind:        project.KindOnce,
			Deps:        []string{"warmup_pull_postgres_image"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"db/bootstrap.sql"}, Env: []string{"DEVFLOW_INSTANCE_ID", "DATABASE_URL"}},
			Outputs:     project.Outputs{Files: []string{".devflow/example/db/identity.json"}},
			Description: "Provision per-worktree database identity",
			Signature:   "ensure-db-v2",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "ensure_db")
				return writeJSONFile(rt, ".devflow/example/db/identity.json", map[string]any{
					"instance":    rt.Instance.ID,
					"database":    rt.Instance.DB.Name,
					"databaseUrl": rt.Instance.DB.URL,
				})
			},
		},
		{
			Name:        "prisma_migrate",
			Kind:        project.KindOnce,
			Deps:        []string{"ensure_db"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"db/schema.prisma"}, Dirs: []string{"db/migrations"}, Env: []string{"DEVFLOW_INSTANCE_ID"}},
			Outputs:     project.Outputs{Files: []string{".devflow/example/db/migrate.json"}},
			Description: "Apply prisma migrations to the local DB",
			Signature:   "prisma-migrate-v2",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "prisma_migrate")
				migrations, err := collectRelativeFiles(rt.Abs("db/migrations"))
				if err != nil {
					return err
				}
				return writeJSONFile(rt, ".devflow/example/db/migrate.json", map[string]any{
					"instance":   rt.Instance.ID,
					"database":   rt.Instance.DB.Name,
					"migrations": migrations,
				})
			},
		},
		{
			Name:        "prisma_generate",
			Kind:        project.KindOnce,
			Deps:        []string{"warmup_node_install"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"db/schema.prisma"}},
			Outputs:     project.Outputs{Files: []string{"backend/generated/prisma-client.json"}},
			Description: "Generate backend prisma client",
			Signature:   "prisma-generate-v2",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "prisma_generate")
				sum, err := fileDigest(rt.Abs("db/schema.prisma"))
				if err != nil {
					return err
				}
				return writeJSONFile(rt, "backend/generated/prisma-client.json", map[string]any{
					"schemaDigest": sum,
					"database":     rt.Instance.DB.Name,
				})
			},
		},
		{
			Name:        "backend_schema",
			Kind:        project.KindOnce,
			Deps:        []string{"prisma_generate"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"backend/sqlc.yaml"}, Dirs: []string{"backend/sql/queries"}},
			Outputs:     project.Outputs{Files: []string{"backend/generated/schema.sql"}},
			Description: "Materialize canonical backend schema inputs",
			Signature:   "backend-schema-v2",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "backend_schema")
				digest, err := dirDigest(rt.Abs("backend/sql/queries"))
				if err != nil {
					return err
				}
				return project.WriteFile(rt, "backend/generated/schema.sql", []byte("-- schema digest: "+digest+"\n"), 0o644)
			},
		},
		{
			Name:        "sqlc_generate_app",
			Kind:        project.KindOnce,
			Deps:        []string{"backend_schema"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"backend/sqlc.yaml"}, Dirs: []string{"backend/sql/queries"}},
			Outputs:     project.Outputs{Files: []string{"backend/generated/sqlc/app.json"}},
			Description: "Generate sqlc application bindings",
			Signature:   "sqlc-generate-app-v2",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "sqlc_generate_app")
				digest, err := dirDigest(rt.Abs("backend/sql/queries"))
				if err != nil {
					return err
				}
				return writeJSONFile(rt, "backend/generated/sqlc/app.json", map[string]any{
					"queryDigest": digest,
				})
			},
		},
		{
			Name:        "auth_mapping",
			Kind:        project.KindOnce,
			Deps:        []string{"backend_schema"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"backend/auth/config.json"}},
			Outputs:     project.Outputs{Files: []string{"backend/generated/auth-mapping.json"}},
			Description: "Generate auth mapping artifacts",
			Signature:   "auth-mapping-v2",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "auth_mapping")
				digest, err := fileDigest(rt.Abs("backend/auth/config.json"))
				if err != nil {
					return err
				}
				return writeJSONFile(rt, "backend/generated/auth-mapping.json", map[string]any{"configDigest": digest})
			},
		},
		{
			Name:        "swagger_external",
			Kind:        project.KindOnce,
			Deps:        []string{"backend_schema"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"backend/openapi/base.json"}},
			Outputs:     project.Outputs{Files: []string{"backend/generated/openapi-external.raw.json"}},
			Description: "Generate raw external swagger output",
			Signature:   "swagger-external-v2",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "swagger_external")
				return writeJSONFile(rt, "backend/generated/openapi-external.raw.json", map[string]any{
					"source": "backend/openapi/base.json",
					"scope":  "external",
				})
			},
		},
		{
			Name:        "swagger_internal",
			Kind:        project.KindOnce,
			Deps:        []string{"backend_schema"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"backend/openapi/base.json"}},
			Outputs:     project.Outputs{Files: []string{"backend/generated/openapi-internal.raw.json"}},
			Description: "Generate raw internal swagger output",
			Signature:   "swagger-internal-v2",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "swagger_internal")
				return writeJSONFile(rt, "backend/generated/openapi-internal.raw.json", map[string]any{
					"source": "backend/openapi/base.json",
					"scope":  "internal",
				})
			},
		},
		{
			Name:        "modifyswagger_external",
			Kind:        project.KindOnce,
			Deps:        []string{"swagger_external"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"backend/openapi/patches/external.json"}},
			Outputs:     project.Outputs{Files: []string{"backend/generated/openapi-external.json"}},
			Description: "Patch external swagger output",
			Signature:   "modifyswagger-external-v2",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "modifyswagger_external")
				digest, err := fileDigest(rt.Abs("backend/openapi/patches/external.json"))
				if err != nil {
					return err
				}
				return writeJSONFile(rt, "backend/generated/openapi-external.json", map[string]any{
					"scope":       "external",
					"patchDigest": digest,
				})
			},
		},
		{
			Name:        "modifyswagger_internal",
			Kind:        project.KindOnce,
			Deps:        []string{"swagger_internal"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"backend/openapi/patches/internal.json"}},
			Outputs:     project.Outputs{Files: []string{"backend/generated/openapi-internal.json"}},
			Description: "Patch internal swagger output",
			Signature:   "modifyswagger-internal-v2",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "modifyswagger_internal")
				digest, err := fileDigest(rt.Abs("backend/openapi/patches/internal.json"))
				if err != nil {
					return err
				}
				return writeJSONFile(rt, "backend/generated/openapi-internal.json", map[string]any{
					"scope":       "internal",
					"patchDigest": digest,
				})
			},
		},
		{
			Name:        "go_generate",
			Kind:        project.KindOnce,
			Deps:        []string{"sqlc_generate_app", "auth_mapping", "modifyswagger_external", "modifyswagger_internal", "prisma_generate"},
			Cache:       true,
			Inputs:      project.Inputs{Dirs: []string{"backend/src"}},
			Outputs:     project.Outputs{Files: []string{"backend/generated/go-generate.json"}},
			Description: "Generate final backend glue code",
			Signature:   "go-generate-v2",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "go_generate")
				digest, err := dirDigest(rt.Abs("backend/src"))
				if err != nil {
					return err
				}
				return writeJSONFile(rt, "backend/generated/go-generate.json", map[string]any{"sourceDigest": digest})
			},
		},
		{
			Name:        "backend_codegen",
			Kind:        project.KindGroup,
			Deps:        []string{"go_generate"},
			Description: "Logical backend codegen aggregate",
		},
		{
			Name:        "frontend_codegen",
			Kind:        project.KindOnce,
			Deps:        []string{"modifyswagger_external", "prisma_generate"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"frontend/codegen.config.json"}},
			Outputs:     project.Outputs{Files: []string{"frontend/generated/api-client.json"}},
			Description: "Generate frontend API client",
			Signature:   "frontend-codegen-v2",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "frontend_codegen")
				digest, err := fileDigest(rt.Abs("frontend/codegen.config.json"))
				if err != nil {
					return err
				}
				return writeJSONFile(rt, "frontend/generated/api-client.json", map[string]any{
					"configDigest": digest,
					"backendPort":  rt.Instance.Ports["backend"],
				})
			},
		},
		{
			Name:         "backend_dev",
			Kind:         project.KindService,
			Deps:         []string{"backend_codegen", "prisma_migrate"},
			Inputs:       project.Inputs{Dirs: []string{"backend/src", "backend/generated"}},
			Restart:      project.RestartOnInputChange,
			Description:  "Run local backend service",
			Signature:    "backend-dev-v2",
			Ready:        project.ReadyFile(".devflow/example/runtime/backend.ready"),
			ReadyTimeout: 3 * time.Second,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "backend_dev")
				env := cloneEnv(rt.Env)
				env["BACKEND_PORT"] = strconv.Itoa(rt.Instance.Ports["backend"])
				env["DATABASE_URL"] = rt.Instance.DB.URL
				readyPath := rt.Abs(".devflow/example/runtime/backend.ready")
				_ = os.Remove(readyPath)
				_, err := rt.StartServiceSpec(ctx, process.CommandSpec{
					Name: "sh",
					Args: []string{"-c", "trap 'rm -f " + shellQuote(readyPath) + "; exit 0' INT TERM; mkdir -p " + shellQuote(filepath.Dir(readyPath)) + "; : > " + shellQuote(readyPath) + "; while true; do echo backend:$BACKEND_PORT:$DATABASE_URL; sleep 1; done"},
					Dir:  rt.Worktree,
					Env:  env,
				})
				return err
			},
		},
		{
			Name:         "frontend_dev",
			Kind:         project.KindService,
			Deps:         []string{"frontend_codegen"},
			Inputs:       project.Inputs{Dirs: []string{"frontend/src", "frontend/generated"}},
			Restart:      project.RestartOnInputChange,
			Description:  "Run local frontend service",
			Signature:    "frontend-dev-v2",
			Ready:        project.ReadyFile(".devflow/example/runtime/frontend.ready"),
			ReadyTimeout: 3 * time.Second,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "frontend_dev")
				env := cloneEnv(rt.Env)
				env["FRONTEND_PORT"] = strconv.Itoa(rt.Instance.Ports["frontend"])
				env["BACKEND_PORT"] = strconv.Itoa(rt.Instance.Ports["backend"])
				readyPath := rt.Abs(".devflow/example/runtime/frontend.ready")
				_ = os.Remove(readyPath)
				_, err := rt.StartServiceSpec(ctx, process.CommandSpec{
					Name: "sh",
					Args: []string{"-c", "trap 'rm -f " + shellQuote(readyPath) + "; exit 0' INT TERM; mkdir -p " + shellQuote(filepath.Dir(readyPath)) + "; : > " + shellQuote(readyPath) + "; while true; do echo frontend:$FRONTEND_PORT:$BACKEND_PORT; sleep 1; done"},
					Dir:  rt.Worktree,
					Env:  env,
				})
				return err
			},
		},
	}
}

func (exampleProject) Targets() []project.Target {
	return []project.Target{
		{
			Name:        "backend-codegen",
			RootTasks:   []string{"backend_codegen"},
			Description: "Run backend code generation",
		},
		{
			Name:        "frontend-setup",
			RootTasks:   []string{"frontend_codegen"},
			Description: "Run frontend code generation setup",
		},
		{
			Name:        "db-only",
			RootTasks:   []string{"prisma_generate"},
			Description: "Prepare local DB artifacts only",
		},
		{
			Name:        "frontend-stack",
			RootTasks:   []string{"backend_dev", "frontend_dev"},
			Description: "Start the example frontend stack",
		},
		{
			Name:        "fullstack",
			RootTasks:   []string{"backend_dev", "frontend_dev"},
			Description: "Alias for the full example stack",
		},
	}
}

func SeedWorktree(dst string) error {
	root, err := fixtureRoot()
	if err != nil {
		return err
	}
	return fsutil.CopyDir(root, dst)
}

func fixtureRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("unable to resolve example fixture root")
	}
	return filepath.Join(filepath.Dir(file), "worktree"), nil
}

func recordTrace(rt *project.Runtime, task string) {
	path := rt.Abs(filepath.Join(".devflow", "example", "traces", task+".log"))
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), rt.Instance.ID)
}

func writeJSONFile(rt *project.Runtime, rel string, payload map[string]any) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return project.WriteFile(rt, rel, data, 0o644)
}

func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func dirDigest(root string) (string, error) {
	files, err := collectRelativeFiles(root)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, rel := range files {
		path := filepath.Join(root, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write([]byte{'\n'})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func collectRelativeFiles(root string) ([]string, error) {
	files := make([]string, 0)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func cloneEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func traceCount(worktree, task string) int {
	data, err := os.ReadFile(filepath.Join(worktree, ".devflow", "example", "traces", task+".log"))
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	count := 0
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}
