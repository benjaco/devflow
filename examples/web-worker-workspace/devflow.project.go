package webworkerworkspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"devflow/pkg/api"
	"devflow/pkg/database"
	"devflow/pkg/process"
	"devflow/pkg/project"
)

type workspaceProject struct{}

func init() {
	project.Register(workspaceProject{})
}

func (workspaceProject) Name() string {
	return "web-worker-workspace"
}

func (workspaceProject) DefaultTarget() string {
	return "fullstack"
}

func (workspaceProject) DetectWorktree(worktree string) bool {
	required := []string{
		"contracts/openapi.json",
		"backend/config/routes.json",
		"worker/config/jobs.json",
	}
	for _, rel := range required {
		if _, err := os.Stat(filepath.Join(worktree, rel)); err != nil {
			return false
		}
	}
	return true
}

func (workspaceProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	dotenv, err := project.LoadOptionalDotEnvInWorktree(worktree, ".env")
	if err != nil {
		return project.InstanceConfig{}, err
	}
	manager := database.New()
	return project.InstanceConfig{
		Label:     filepath.Base(worktree),
		PortNames: []string{"backend", "frontend", "worker", "postgres"},
		Env: project.MergeEnvMaps(dotenv, map[string]string{
			"DEVFLOW_EXAMPLE_PROJECT": "web-worker-workspace",
			"APP_ENV":                 "development",
		}),
		Finalize: func(inst *api.Instance) error {
			db := manager.Desired(inst.ID, database.Config{
				HostPort:     inst.Ports["postgres"],
				Database:     fmt.Sprintf("workspace_%s", inst.ID),
				User:         "devflow",
				Password:     "devflow",
				SnapshotRoot: filepath.Join(inst.Worktree, ".devflow", "web-worker-workspace", "dbsnapshots"),
			})
			inst.DB = db
			inst.Env = project.MergeEnvMaps(inst.Env, map[string]string{
				"BACKEND_PORT":             strconv.Itoa(inst.Ports["backend"]),
				"FRONTEND_PORT":            strconv.Itoa(inst.Ports["frontend"]),
				"WORKER_PORT":              strconv.Itoa(inst.Ports["worker"]),
				"PGHOST":                   db.Host,
				"PGPORT":                   strconv.Itoa(db.Port),
				"PGDATABASE":               db.Name,
				"PGUSER":                   db.User,
				"PGPASSWORD":               db.Password,
				"DATABASE_URL":             db.URL,
				"NEXT_PUBLIC_API_BASE_URL": fmt.Sprintf("http://127.0.0.1:%d", inst.Ports["backend"]),
			})
			return nil
		},
	}, nil
}

func (workspaceProject) Tasks() []project.Task {
	return []project.Task{
		project.ShellTask(
			"warmup_node_install",
			"Warm frontend package install metadata",
			project.KindWarmup,
			nil,
			false,
			project.Outputs{},
			project.Inputs{Files: []string{"frontend/package-lock.json"}},
			"mkdir -p .devflow/web-worker-workspace/warmup && printf 'node warmup\\n' > .devflow/web-worker-workspace/warmup/node.txt",
		),
		project.ShellTask(
			"warmup_worker_cache",
			"Warm worker toolchain metadata",
			project.KindWarmup,
			nil,
			false,
			project.Outputs{},
			project.Inputs{Files: []string{"worker/config/jobs.json"}},
			"mkdir -p .devflow/web-worker-workspace/warmup && printf 'worker warmup\\n' > .devflow/web-worker-workspace/warmup/worker.txt",
		),
		{
			Name:        "prepare_db_base",
			Kind:        project.KindOnce,
			Deps:        []string{"warmup_worker_cache"},
			Inputs:      project.Inputs{Files: []string{"db/schema.prisma", "db/bootstrap.sql"}, Dirs: []string{"db/migrations"}, Env: []string{"DEVFLOW_INSTANCE_ID", "DATABASE_URL"}},
			Outputs:     project.Outputs{Files: []string{".devflow/web-worker-workspace/db/prepare.json"}},
			Description: "Restore nearest DB snapshot or reset to a fresh dedicated volume",
			Signature:   "web-worker-prepare-db-base-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "prepare_db_base")
				state, err := database.InspectPrismaState(rt.Worktree, "db/schema.prisma", "db/migrations", []string{"db/bootstrap.sql"})
				if err != nil {
					return err
				}
				manager := database.New()
				var restored *database.PrismaRestoreResult
				if useFakeDB() {
					plan, err := database.PlanPrismaRestore(rt.Instance.DB.SnapshotRoot, state)
					if err != nil {
						return err
					}
					if plan.SnapshotKey != "" {
						restored = &database.PrismaRestoreResult{Plan: plan, Metadata: plan.Snapshot}
					}
				} else {
					restored, err = manager.RestoreNearestPrismaSnapshot(ctx, rt.Instance.DB, state)
					if err != nil {
						return err
					}
					if restored == nil {
						if err := manager.DestroyRuntime(ctx, rt.Instance.DB, true); err != nil {
							return err
						}
					}
				}
				return writeJSONFile(rt, ".devflow/web-worker-workspace/db/prepare.json", buildPreparePayload(rt, state, restored))
			},
		},
		{
			Name:        "prepare_db_runtime",
			Kind:        project.KindOnce,
			Deps:        []string{"prepare_db_base"},
			Inputs:      project.Inputs{Files: []string{".devflow/web-worker-workspace/db/prepare.json"}},
			Outputs:     project.Outputs{Files: []string{".devflow/web-worker-workspace/db/runtime.json"}},
			Description: "Start the temporary DB runtime used during DB preparation",
			Signature:   "web-worker-prepare-db-runtime-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "prepare_db_runtime")
				if useFakeDB() {
					return writeJSONFile(rt, ".devflow/web-worker-workspace/db/runtime.json", map[string]any{
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
				return writeJSONFile(rt, ".devflow/web-worker-workspace/db/runtime.json", map[string]any{
					"container": rt.Instance.DB.ContainerName,
					"fake":      false,
					"port":      rt.Instance.DB.Port,
				})
			},
		},
		{
			Name:        "db_migrate",
			Kind:        project.KindOnce,
			Deps:        []string{"prepare_db_runtime"},
			Inputs:      project.Inputs{Files: []string{"db/schema.prisma", "db/bootstrap.sql", ".devflow/web-worker-workspace/db/prepare.json"}, Dirs: []string{"db/migrations"}},
			Outputs:     project.Outputs{Files: []string{".devflow/web-worker-workspace/db/migrate.json"}},
			Description: "Apply remaining DB migrations to the local dedicated runtime",
			Signature:   "web-worker-db-migrate-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "db_migrate")
				prepare, err := loadPrepareState(rt)
				if err != nil {
					return err
				}
				remaining := remainingMigrationNames(prepare.State.Migrations, prepare.PrefixLength)
				applied := append([]string(nil), remaining...)
				bootstrapped := !prepare.Restored && prepare.PrefixLength == 0
				if !useFakeDB() {
					if bootstrapped {
						if err := pipeSQLFile(ctx, rt, "db/bootstrap.sql"); err != nil {
							return err
						}
					}
					for _, rel := range remaining {
						if err := pipeSQLFile(ctx, rt, filepath.Join("db/migrations", rel)); err != nil {
							return err
						}
					}
				}
				return writeJSONFile(rt, ".devflow/web-worker-workspace/db/migrate.json", map[string]any{
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
			Deps:        []string{"db_migrate"},
			Inputs:      project.Inputs{Files: []string{".devflow/web-worker-workspace/db/prepare.json", ".devflow/web-worker-workspace/db/migrate.json", "db/schema.prisma", "db/bootstrap.sql"}, Dirs: []string{"db/migrations"}},
			Outputs:     project.Outputs{Files: []string{".devflow/web-worker-workspace/db/snapshot.json"}},
			Description: "Snapshot the prepared DB state",
			Signature:   "web-worker-snapshot-db-state-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "snapshot_db_state")
				prepare, err := loadPrepareState(rt)
				if err != nil {
					return err
				}
				migrateResult, err := loadJSONMap(rt, ".devflow/web-worker-workspace/db/migrate.json")
				if err != nil {
					return err
				}
				state, err := database.InspectPrismaState(rt.Worktree, "db/schema.prisma", "db/migrations", []string{"db/bootstrap.sql"})
				if err != nil {
					return err
				}
				appliedCount := len(stringList(migrateResult["applied"]))
				if useFakeDB() {
					if !prepare.ExactMatch || appliedCount > 0 || !prepare.Restored {
						if _, err := database.SavePrismaSnapshot(rt.Instance.DB.SnapshotRoot, state.FullHash, state); err != nil {
							return err
						}
					}
					return writeJSONFile(rt, ".devflow/web-worker-workspace/db/snapshot.json", map[string]any{
						"key":        state.FullHash,
						"reused":     prepare.ExactMatch && appliedCount == 0 && prepare.Restored,
						"fake":       true,
						"migrations": len(state.Migrations),
					})
				}
				if prepare.ExactMatch && appliedCount == 0 && prepare.Restored {
					return writeJSONFile(rt, ".devflow/web-worker-workspace/db/snapshot.json", map[string]any{
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
				return writeJSONFile(rt, ".devflow/web-worker-workspace/db/snapshot.json", map[string]any{
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
			Description:  "Run the dedicated Postgres runtime for this workspace",
			Signature:    "web-worker-postgres-v1",
			Ready:        dbReady,
			ReadyTimeout: 30 * time.Second,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "postgres")
				if useFakeDB() {
					readyPath := rt.Abs(".devflow/web-worker-workspace/runtime/postgres.ready")
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
			Name:        "contract_codegen",
			Kind:        project.KindOnce,
			Deps:        []string{"warmup_node_install"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"contracts/openapi.json"}},
			Outputs:     project.Outputs{Files: []string{"generated/contracts/api.json"}},
			Description: "Generate the shared API contract artifact",
			Signature:   "web-worker-contract-codegen-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "contract_codegen")
				sum, err := fileDigest(rt.Abs("contracts/openapi.json"))
				if err != nil {
					return err
				}
				return writeJSONFile(rt, "generated/contracts/api.json", map[string]any{
					"contractDigest": sum,
					"instance":       rt.Instance.ID,
				})
			},
		},
		{
			Name:        "backend_codegen",
			Kind:        project.KindOnce,
			Deps:        []string{"contract_codegen"},
			Cache:       true,
			Inputs:      project.Inputs{Dirs: []string{"backend/src"}, Files: []string{"backend/config/routes.json"}},
			Outputs:     project.Outputs{Files: []string{"backend/generated/router.json"}},
			Description: "Generate the backend router surface",
			Signature:   "web-worker-backend-codegen-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "backend_codegen")
				srcDigest, err := dirDigest(rt.Abs("backend/src"))
				if err != nil {
					return err
				}
				contract, err := fileDigest(rt.Abs("contracts/openapi.json"))
				if err != nil {
					return err
				}
				return writeJSONFile(rt, "backend/generated/router.json", map[string]any{
					"sourceDigest":   srcDigest,
					"contractDigest": contract,
				})
			},
		},
		{
			Name:        "frontend_codegen",
			Kind:        project.KindOnce,
			Deps:        []string{"contract_codegen"},
			Cache:       true,
			Inputs:      project.Inputs{Files: []string{"frontend/codegen.config.json"}},
			Outputs:     project.Outputs{Files: []string{"frontend/generated/client.json"}},
			Description: "Generate the frontend API client",
			Signature:   "web-worker-frontend-codegen-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "frontend_codegen")
				configDigest, err := fileDigest(rt.Abs("frontend/codegen.config.json"))
				if err != nil {
					return err
				}
				contract, err := fileDigest(rt.Abs("contracts/openapi.json"))
				if err != nil {
					return err
				}
				return writeJSONFile(rt, "frontend/generated/client.json", map[string]any{
					"configDigest":   configDigest,
					"contractDigest": contract,
					"backendPort":    rt.Instance.Ports["backend"],
				})
			},
		},
		{
			Name:        "worker_bundle",
			Kind:        project.KindOnce,
			Deps:        []string{"warmup_worker_cache", "contract_codegen"},
			Cache:       true,
			Inputs:      project.Inputs{Dirs: []string{"worker/src"}, Files: []string{"worker/config/jobs.json"}},
			Outputs:     project.Outputs{Files: []string{"worker/generated/bundle.json"}},
			Description: "Bundle the background worker runtime",
			Signature:   "web-worker-worker-bundle-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				recordTrace(rt, "worker_bundle")
				srcDigest, err := dirDigest(rt.Abs("worker/src"))
				if err != nil {
					return err
				}
				jobsDigest, err := fileDigest(rt.Abs("worker/config/jobs.json"))
				if err != nil {
					return err
				}
				return writeJSONFile(rt, "worker/generated/bundle.json", map[string]any{
					"sourceDigest": srcDigest,
					"jobsDigest":   jobsDigest,
				})
			},
		},
		{
			Name:        "codegen",
			Kind:        project.KindGroup,
			Deps:        []string{"backend_codegen", "frontend_codegen", "worker_bundle"},
			Description: "Aggregate contract, app, and worker build outputs",
		},
		{
			Name:         "backend_dev",
			Kind:         project.KindService,
			Deps:         []string{"backend_codegen", "postgres"},
			Inputs:       project.Inputs{Dirs: []string{"backend/src", "backend/generated"}},
			Restart:      project.RestartOnInputChange,
			Description:  "Run the local API service",
			Signature:    "web-worker-backend-dev-v1",
			Ready:        project.ReadyFile(".devflow/web-worker-workspace/runtime/backend.ready"),
			ReadyTimeout: 3 * time.Second,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "backend_dev")
				readyPath := rt.Abs(".devflow/web-worker-workspace/runtime/backend.ready")
				_ = os.Remove(readyPath)
				_, err := rt.StartServiceSpec(ctx, process.CommandSpec{
					Name: "sh",
					Args: []string{"-c", "trap 'rm -f " + shellQuote(readyPath) + "; exit 0' INT TERM; mkdir -p " + shellQuote(filepath.Dir(readyPath)) + "; : > " + shellQuote(readyPath) + "; while true; do echo backend:$BACKEND_PORT:$DATABASE_URL:$API_PUBLIC_FLAG; sleep 1; done"},
					Dir:  rt.Worktree,
					Env:  cloneEnv(rt.Env),
				})
				return err
			},
		},
		{
			Name:         "worker_dev",
			Kind:         project.KindService,
			Deps:         []string{"worker_bundle", "postgres"},
			Inputs:       project.Inputs{Dirs: []string{"worker/src", "worker/generated"}},
			Restart:      project.RestartOnInputChange,
			Description:  "Run the local background worker",
			Signature:    "web-worker-worker-dev-v1",
			Ready:        project.ReadyFile(".devflow/web-worker-workspace/runtime/worker.ready"),
			ReadyTimeout: 3 * time.Second,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "worker_dev")
				readyPath := rt.Abs(".devflow/web-worker-workspace/runtime/worker.ready")
				_ = os.Remove(readyPath)
				_, err := rt.StartServiceSpec(ctx, process.CommandSpec{
					Name: "sh",
					Args: []string{"-c", "trap 'rm -f " + shellQuote(readyPath) + "; exit 0' INT TERM; mkdir -p " + shellQuote(filepath.Dir(readyPath)) + "; : > " + shellQuote(readyPath) + "; while true; do echo worker:$WORKER_PORT:$QUEUE_NAME:$DATABASE_URL; sleep 1; done"},
					Dir:  rt.Worktree,
					Env:  cloneEnv(rt.Env),
				})
				return err
			},
		},
		{
			Name:         "frontend_dev",
			Kind:         project.KindService,
			Deps:         []string{"frontend_codegen", "backend_dev"},
			Inputs:       project.Inputs{Dirs: []string{"frontend/src", "frontend/generated"}},
			Restart:      project.RestartOnInputChange,
			Description:  "Run the local web frontend",
			Signature:    "web-worker-frontend-dev-v1",
			Ready:        project.ReadyFile(".devflow/web-worker-workspace/runtime/frontend.ready"),
			ReadyTimeout: 3 * time.Second,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				recordTrace(rt, "frontend_dev")
				readyPath := rt.Abs(".devflow/web-worker-workspace/runtime/frontend.ready")
				_ = os.Remove(readyPath)
				_, err := rt.StartServiceSpec(ctx, process.CommandSpec{
					Name: "sh",
					Args: []string{"-c", "trap 'rm -f " + shellQuote(readyPath) + "; exit 0' INT TERM; mkdir -p " + shellQuote(filepath.Dir(readyPath)) + "; : > " + shellQuote(readyPath) + "; while true; do echo frontend:$FRONTEND_PORT:$NEXT_PUBLIC_API_BASE_URL:$WEB_UI_THEME; sleep 1; done"},
					Dir:  rt.Worktree,
					Env:  cloneEnv(rt.Env),
				})
				return err
			},
		},
	}
}

func (workspaceProject) Targets() []project.Target {
	return []project.Target{
		{
			Name:        "codegen",
			RootTasks:   []string{"codegen"},
			Description: "Run shared contract and app code generation",
		},
		{
			Name:        "db-only",
			RootTasks:   []string{"postgres"},
			Description: "Prepare and run the dedicated Postgres runtime",
		},
		{
			Name:        "services",
			RootTasks:   []string{"backend_dev", "worker_dev"},
			Description: "Run the API and background worker services",
		},
		{
			Name:        "fullstack",
			RootTasks:   []string{"backend_dev", "worker_dev", "frontend_dev"},
			Description: "Run the full web and worker workspace",
		},
	}
}

func SeedWorktree(dst string) error {
	files := map[string]string{
		".env":                         "API_PUBLIC_FLAG=public-dev\nQUEUE_NAME=emails\nWEB_UI_THEME=copper\n",
		"db/schema.prisma":             "datasource db { provider = \"postgresql\" url = env(\"DATABASE_URL\") }\nmodel Widget { id Int @id }\n",
		"db/bootstrap.sql":             "create table bootstrap_marker(id integer primary key);\n",
		"db/migrations/001_init.sql":   "create table jobs(id integer primary key, payload text not null);\n",
		"contracts/openapi.json":       "{ \"openapi\": \"3.0.0\", \"info\": { \"title\": \"workspace-api\", \"version\": \"1.0.0\" } }\n",
		"backend/config/routes.json":   "{ \"routes\": [\"/health\", \"/jobs\"] }\n",
		"backend/src/server.go":        "package backend\nvar Version = \"v1\"\n",
		"frontend/package-lock.json":   "{ \"lockfileVersion\": 3 }\n",
		"frontend/codegen.config.json": "{ \"generator\": \"typed-client\" }\n",
		"frontend/src/app.tsx":         "export const App = () => 'hello';\n",
		"worker/config/jobs.json":      "{ \"jobs\": [\"emails\", \"cleanup\"] }\n",
		"worker/src/worker.go":         "package worker\nvar Queue = \"emails\"\n",
	}
	for rel, contents := range files {
		path := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			return err
		}
	}
	return nil
}

type prepareState struct {
	SnapshotKey  string
	PrefixLength int
	Restored     bool
	ExactMatch   bool
	State        database.PrismaState
}

func buildPreparePayload(rt *project.Runtime, state *database.PrismaState, restored *database.PrismaRestoreResult) map[string]any {
	payload := map[string]any{
		"database":      rt.Instance.DB.Name,
		"snapshotKey":   "",
		"prefixLength":  0,
		"restored":      false,
		"exactMatch":    false,
		"state":         state,
		"containerName": rt.Instance.DB.ContainerName,
		"volumeName":    rt.Instance.DB.VolumeName,
	}
	if restored != nil {
		payload["snapshotKey"] = restored.Plan.SnapshotKey
		payload["prefixLength"] = restored.Plan.PrefixLength
		payload["restored"] = restored.Plan.SnapshotKey != ""
		payload["exactMatch"] = restored.Plan.ExactMatch
	}
	return payload
}

func loadPrepareState(rt *project.Runtime) (*prepareState, error) {
	values, err := loadJSONMap(rt, ".devflow/web-worker-workspace/db/prepare.json")
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
		SnapshotKey:  stringValue(values["snapshotKey"]),
		PrefixLength: intValue(values["prefixLength"]),
		Restored:     boolValue(values["restored"]),
		ExactMatch:   boolValue(values["exactMatch"]),
		State:        prismaState,
	}, nil
}

func useFakeDB() bool {
	return os.Getenv("DEVFLOW_WEBWORKER_FAKE_DB") == "1"
}

func dbReady(ctx context.Context, rt *project.Runtime) error {
	if useFakeDB() {
		return project.ReadyFile(".devflow/web-worker-workspace/runtime/postgres.ready")(ctx, rt)
	}
	return database.New().WaitReady(ctx, rt.Instance.DB, 30*time.Second)
}

func recordTrace(rt *project.Runtime, task string) {
	path := rt.Abs(filepath.Join(".devflow", "web-worker-workspace", "traces", task+".log"))
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
	data, err := os.ReadFile(filepath.Join(worktree, ".devflow", "web-worker-workspace", "traces", task+".log"))
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
