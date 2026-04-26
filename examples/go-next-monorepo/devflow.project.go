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

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/database"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

type exampleProject struct{}

func init() {
	project.Register(exampleProject{})
}

func (exampleProject) Name() string {
	return "go-next-monorepo"
}

func (exampleProject) DefaultTarget() string {
	return "fullstack"
}

func (exampleProject) DetectWorktree(worktree string) bool {
	required := []string{
		"backend/sqlc.yaml",
		"frontend/codegen.config.json",
		"db/schema.prisma",
	}
	for _, rel := range required {
		if _, err := os.Stat(filepath.Join(worktree, rel)); err != nil {
			return false
		}
	}
	return true
}

func (exampleProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	id, _, err := instance.IDForWorktree(worktree)
	if err != nil {
		return project.InstanceConfig{}, err
	}
	dotenv, err := project.LoadOptionalDotEnvInWorktree(worktree, ".env")
	if err != nil {
		return project.InstanceConfig{}, err
	}
	manager := database.New()
	return project.InstanceConfig{
		Label:     filepath.Base(worktree),
		PortNames: []string{"backend", "frontend", "postgres"},
		Env: project.MergeEnvMaps(dotenv, map[string]string{
			"DEVFLOW_EXAMPLE_PROJECT": "go-next-monorepo",
		}),
		Finalize: func(inst *api.Instance) error {
			db := manager.Desired(inst.ID, database.Config{
				HostPort:     inst.Ports["postgres"],
				Database:     fmt.Sprintf("app_wt_%s", id),
				User:         "devflow",
				Password:     "devflow",
				SnapshotRoot: filepath.Join(inst.Worktree, ".devflow", "dbsnapshots"),
			})
			inst.DB = db
			if inst.Env == nil {
				inst.Env = map[string]string{}
			}
			inst.Env = project.MergeEnvMaps(inst.Env, map[string]string{
				"PGHOST":       db.Host,
				"PGPORT":       strconv.Itoa(db.Port),
				"PGDATABASE":   db.Name,
				"PGUSER":       db.User,
				"PGPASSWORD":   db.Password,
				"DATABASE_URL": db.URL,
			})
			return nil
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
			Name:        "prepare_db_base",
			Kind:        project.KindOnce,
			Deps:        []string{"warmup_pull_postgres_image"},
			Inputs:      project.Inputs{Files: []string{"db/schema.prisma", "db/bootstrap.sql"}, Dirs: []string{"db/migrations"}, Env: []string{"DEVFLOW_INSTANCE_ID", "DATABASE_URL"}},
			Outputs:     project.Outputs{Files: []string{".devflow/example/db/prepare.json"}},
			Description: "Restore the nearest DB snapshot or recreate from the configured base source",
			Signature:   "prepare-db-base-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "prepare_db_base")
				state, err := database.InspectPrismaState(rt.Worktree, "db/schema.prisma", "db/migrations", []string{"db/bootstrap.sql"})
				if err != nil {
					return err
				}
				var prepared *database.PrismaBaseResult
				if exampleUseFakeDB() {
					plan, err := database.PlanPrismaRestore(rt.Instance.DB.SnapshotRoot, state)
					if err != nil {
						return err
					}
					if plan.SnapshotKey != "" {
						prepared = &database.PrismaBaseResult{
							Restored: &database.PrismaRestoreResult{Plan: plan, Metadata: plan.Snapshot},
						}
					} else {
						prepared = &database.PrismaBaseResult{Recreated: true}
					}
				} else {
					prepared, err = database.New().PreparePrismaBase(ctx, rt.Instance.DB, state, nil, database.PrepareOptions{
						Worktree: rt.Worktree,
						Env:      cloneEnv(rt.Env),
						LogPath:  rt.LogPath,
						OnLine: func(stream, line string) {
							if rt.EventFn == nil {
								return
							}
							rt.EventFn(api.Event{
								TS:         process.NowRFC3339Nano(),
								Type:       api.EventLogLine,
								InstanceID: rt.Instance.ID,
								Worktree:   rt.Worktree,
								Task:       rt.TaskName,
								Mode:       rt.Mode,
								Stream:     stream,
								Line:       line,
							})
						},
					})
					if err != nil {
						return err
					}
				}
				payload := buildPreparePayload(rt, state, prepared)
				return writeJSONFile(rt, ".devflow/example/db/prepare.json", payload)
			},
		},
		{
			Name:        "prepare_db_runtime",
			Kind:        project.KindOnce,
			Deps:        []string{"prepare_db_base"},
			Inputs:      project.Inputs{Files: []string{".devflow/example/db/prepare.json"}},
			Outputs:     project.Outputs{Files: []string{".devflow/example/db/runtime.json"}},
			Description: "Start the temporary Postgres runtime used for DB preparation",
			Signature:   "prepare-db-runtime-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "prepare_db_runtime")
				if exampleUseFakeDB() {
					return writeJSONFile(rt, ".devflow/example/db/runtime.json", map[string]any{
						"container": rt.Instance.DB.ContainerName,
						"fake":      true,
						"port":      rt.Instance.DB.Port,
					})
				}
				manager := database.New()
				if err := manager.EnsureRuntime(ctx, rt.Instance.DB); err != nil {
					return err
				}
				if err := manager.WaitReady(ctx, rt.Instance.DB, 30*time.Second); err != nil {
					return err
				}
				return writeJSONFile(rt, ".devflow/example/db/runtime.json", map[string]any{
					"container": rt.Instance.DB.ContainerName,
					"fake":      false,
					"port":      rt.Instance.DB.Port,
				})
			},
		},
		{
			Name:        "prisma_migrate",
			Kind:        project.KindOnce,
			Deps:        []string{"prepare_db_runtime"},
			Inputs:      project.Inputs{Files: []string{"db/schema.prisma", "db/bootstrap.sql", ".devflow/example/db/prepare.json"}, Dirs: []string{"db/migrations"}, Env: []string{"DEVFLOW_INSTANCE_ID"}},
			Outputs:     project.Outputs{Files: []string{".devflow/example/db/migrate.json"}},
			Description: "Apply prisma migrations to the local DB",
			Signature:   "prisma-migrate-v3",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "prisma_migrate")
				prepare, err := loadPrepareState(rt)
				if err != nil {
					return err
				}
				remaining := remainingMigrationNames(prepare.State.Migrations, prepare.PrefixLength)
				applied := append([]string(nil), remaining...)
				bootstrapped := !prepare.Restored && prepare.PrefixLength == 0
				if !exampleUseFakeDB() {
					if bootstrapped {
						if err := pipeSQLFile(ctx, rt, "db/bootstrap.sql"); err != nil {
							return err
						}
					}
					for _, rel := range remaining {
						if err := pipeSQLFile(ctx, rt, filepath.Join("db/migrations", rel, "migration.sql")); err != nil {
							return err
						}
					}
				}
				return writeJSONFile(rt, ".devflow/example/db/migrate.json", map[string]any{
					"instance":      rt.Instance.ID,
					"database":      rt.Instance.DB.Name,
					"applied":       applied,
					"prefixLength":  prepare.PrefixLength,
					"snapshotKey":   prepare.SnapshotKey,
					"restored":      prepare.Restored,
					"exactMatch":    prepare.ExactMatch,
					"bootstrapped":  bootstrapped,
					"targetFullKey": prepare.State.FullHash,
				})
			},
		},
		{
			Name:        "snapshot_db_state",
			Kind:        project.KindOnce,
			Deps:        []string{"prisma_migrate"},
			Inputs:      project.Inputs{Files: []string{".devflow/example/db/prepare.json", ".devflow/example/db/migrate.json", "db/schema.prisma", "db/bootstrap.sql"}, Dirs: []string{"db/migrations"}},
			Outputs:     project.Outputs{Files: []string{".devflow/example/db/snapshot.json"}},
			Description: "Snapshot the prepared DB state for future restore",
			Signature:   "snapshot-db-state-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "snapshot_db_state")
				prepare, err := loadPrepareState(rt)
				if err != nil {
					return err
				}
				migrateResult, err := loadJSONMap(rt, ".devflow/example/db/migrate.json")
				if err != nil {
					return err
				}
				state, err := database.InspectPrismaState(rt.Worktree, "db/schema.prisma", "db/migrations", []string{"db/bootstrap.sql"})
				if err != nil {
					return err
				}
				appliedCount := len(stringList(migrateResult["applied"]))
				if exampleUseFakeDB() {
					if !prepare.ExactMatch || appliedCount > 0 || !prepare.Restored {
						if _, err := database.SavePrismaSnapshot(rt.Instance.DB.SnapshotRoot, state.FullHash, state); err != nil {
							return err
						}
					}
					return writeJSONFile(rt, ".devflow/example/db/snapshot.json", map[string]any{
						"key":        state.FullHash,
						"reused":     prepare.ExactMatch && appliedCount == 0 && prepare.Restored,
						"fake":       true,
						"migrations": len(state.Migrations),
					})
				}
				if prepare.ExactMatch && appliedCount == 0 && prepare.Restored {
					return writeJSONFile(rt, ".devflow/example/db/snapshot.json", map[string]any{
						"key":        prepare.SnapshotKey,
						"reused":     true,
						"fake":       false,
						"migrations": len(state.Migrations),
					})
				}
				result, err := database.New().SnapshotPrisma(ctx, rt.Instance.DB, state.FullHash, state)
				if err != nil {
					return err
				}
				return writeJSONFile(rt, ".devflow/example/db/snapshot.json", map[string]any{
					"key":        result.Plan.SnapshotKey,
					"reused":     false,
					"fake":       false,
					"migrations": len(state.Migrations),
				})
			},
		},
		{
			Name:         "postgres",
			Kind:         project.KindService,
			Deps:         []string{"snapshot_db_state"},
			Restart:      project.RestartOnInputChange,
			Description:  "Run the dedicated Postgres runtime for this worktree",
			Signature:    "postgres-runtime-v1",
			Ready:        exampleDBReady,
			ReadyTimeout: 30 * time.Second,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "postgres")
				if exampleUseFakeDB() {
					readyPath := rt.Abs(".devflow/example/runtime/postgres.ready")
					_ = os.Remove(readyPath)
					_, err := rt.StartServiceSpec(ctx, process.CommandSpec{
						Name: "sh",
						Args: []string{"-c", "trap 'rm -f " + shellQuote(readyPath) + "; exit 0' INT TERM; mkdir -p " + shellQuote(filepath.Dir(readyPath)) + "; : > " + shellQuote(readyPath) + "; while true; do echo postgres:$PGPORT:$PGDATABASE; sleep 1; done"},
						Dir:  rt.Worktree,
						Env:  cloneEnv(rt.Env),
					})
					return err
				}
				manager := database.New()
				if err := manager.EnsureRuntime(ctx, rt.Instance.DB); err != nil {
					return err
				}
				env := cloneEnv(rt.Env)
				env["DB_CONTAINER"] = rt.Instance.DB.ContainerName
				_, err := rt.StartServiceSpec(ctx, process.CommandSpec{
					Name: "sh",
					Args: []string{"-c", "trap 'docker stop -t 10 \"$DB_CONTAINER\" >/dev/null 2>&1 || true; exit 0' INT TERM; docker logs -f \"$DB_CONTAINER\""},
					Dir:  rt.Worktree,
					Env:  env,
				})
				return err
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
			Name:                      "backend_dev",
			Kind:                      project.KindService,
			Deps:                      []string{"backend_codegen", "postgres"},
			Inputs:                    project.Inputs{Dirs: []string{"backend/src", "backend/generated"}},
			Restart:                   project.RestartOnInputChange,
			WatchRestartOnServiceDeps: true,
			Description:               "Run local backend service",
			Signature:                 "backend-dev-v2",
			Ready:                     project.ReadyFile(".devflow/example/runtime/backend.ready"),
			ReadyTimeout:              3 * time.Second,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "backend_dev")
				env := cloneEnv(rt.Env)
				env["BACKEND_PORT"] = strconv.Itoa(rt.Instance.Ports["backend"])
				env["DATABASE_URL"] = rt.Instance.DB.URL
				readyPath := rt.Abs(".devflow/example/runtime/backend.ready")
				_ = os.Remove(readyPath)
				_, err := rt.StartServiceSpec(ctx, process.CommandSpec{
					Name: "sh",
					Args: []string{"-c", "trap 'rm -f " + shellQuote(readyPath) + "; exit 0' INT TERM; mkdir -p " + shellQuote(filepath.Dir(readyPath)) + "; : > " + shellQuote(readyPath) + "; while true; do echo backend:$BACKEND_PORT:$DATABASE_URL:$EXAMPLE_BACKEND_FLAG; sleep 1; done"},
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
					Args: []string{"-c", "trap 'rm -f " + shellQuote(readyPath) + "; exit 0' INT TERM; mkdir -p " + shellQuote(filepath.Dir(readyPath)) + "; : > " + shellQuote(readyPath) + "; while true; do echo frontend:$FRONTEND_PORT:$BACKEND_PORT:$NEXTAUTH_URL:$EXAMPLE_FRONTEND_FLAG; sleep 1; done"},
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
			RootTasks:   []string{"postgres"},
			Description: "Prepare and run the dedicated local Postgres runtime",
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

type prepareState struct {
	SnapshotKey   string
	PrefixLength  int
	Restored      bool
	ExactMatch    bool
	Recreated     bool
	SourceApplied bool
	SourcePolicy  string
	State         database.PrismaState
}

func buildPreparePayload(rt *project.Runtime, state *database.PrismaState, prepared *database.PrismaBaseResult) map[string]any {
	payload := map[string]any{
		"database":      rt.Instance.DB.Name,
		"snapshotKey":   "",
		"prefixLength":  0,
		"restored":      false,
		"exactMatch":    false,
		"recreated":     false,
		"sourceApplied": false,
		"sourcePolicy":  "",
		"state":         state,
		"containerName": rt.Instance.DB.ContainerName,
		"volumeName":    rt.Instance.DB.VolumeName,
	}
	if prepared != nil {
		payload["recreated"] = prepared.Recreated
		payload["sourceApplied"] = prepared.SourceApplied
		payload["sourcePolicy"] = prepared.SourcePolicy
		if prepared.Restored != nil {
			payload["snapshotKey"] = prepared.Restored.Plan.SnapshotKey
			payload["prefixLength"] = prepared.Restored.Plan.PrefixLength
			payload["restored"] = prepared.Restored.Plan.SnapshotKey != ""
			payload["exactMatch"] = prepared.Restored.Plan.ExactMatch
		}
	}
	return payload
}

func loadPrepareState(rt *project.Runtime) (*prepareState, error) {
	values, err := loadJSONMap(rt, ".devflow/example/db/prepare.json")
	if err != nil {
		return nil, err
	}
	rawState, err := json.Marshal(values["state"])
	if err != nil {
		return nil, err
	}
	var prismaState database.PrismaState
	if err := json.Unmarshal(rawState, &prismaState); err != nil {
		return nil, err
	}
	return &prepareState{
		SnapshotKey:   stringValue(values["snapshotKey"]),
		PrefixLength:  intValue(values["prefixLength"]),
		Restored:      boolValue(values["restored"]),
		ExactMatch:    boolValue(values["exactMatch"]),
		Recreated:     boolValue(values["recreated"]),
		SourceApplied: boolValue(values["sourceApplied"]),
		SourcePolicy:  stringValue(values["sourcePolicy"]),
		State:         prismaState,
	}, nil
}

func loadJSONMap(rt *project.Runtime, rel string) (map[string]any, error) {
	data, err := os.ReadFile(rt.Abs(rel))
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func remainingMigrationNames(migrations []database.PrismaMigration, prefixLength int) []string {
	if prefixLength < 0 {
		prefixLength = 0
	}
	if prefixLength > len(migrations) {
		prefixLength = len(migrations)
	}
	out := make([]string, 0, len(migrations)-prefixLength)
	for _, migration := range migrations[prefixLength:] {
		out = append(out, migration.Name)
	}
	return out
}

func exampleUseFakeDB() bool {
	return os.Getenv("DEVFLOW_EXAMPLE_FAKE_DB") == "1"
}

func exampleDBReady(ctx context.Context, rt *project.Runtime) error {
	if exampleUseFakeDB() {
		return project.ReadyFile(".devflow/example/runtime/postgres.ready")(ctx, rt)
	}
	return database.New().WaitReady(ctx, rt.Instance.DB, 30*time.Second)
}

func pipeSQLFile(ctx context.Context, rt *project.Runtime, rel string) error {
	sqlFile := rt.Abs(rel)
	return rt.RunCmdSpec(ctx, process.CommandSpec{
		Name: "sh",
		Args: []string{"-c", "cat \"$SQL_FILE\" | docker exec -i \"$DB_CONTAINER\" psql -U \"$PGUSER\" -d \"$PGDATABASE\" -v ON_ERROR_STOP=1"},
		Dir:  rt.Worktree,
		Env: mergeStringMaps(rt.Env, map[string]string{
			"SQL_FILE":     sqlFile,
			"DB_CONTAINER": rt.Instance.DB.ContainerName,
			"PGUSER":       rt.Instance.DB.User,
			"PGDATABASE":   rt.Instance.DB.Name,
		}),
	})
}

func mergeStringMaps(base, overlay map[string]string) map[string]string {
	out := cloneEnv(base)
	for key, value := range overlay {
		out[key] = value
	}
	return out
}

func stringList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, stringValue(item))
	}
	return out
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func intValue(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

func boolValue(value any) bool {
	flag, _ := value.(bool)
	return flag
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
