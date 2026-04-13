package bikecoach

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"devflow/pkg/api"
	"devflow/pkg/database"
	"devflow/pkg/process"
	"devflow/pkg/project"
)

type bikecoachProject struct{}

func init() {
	project.Register(bikecoachProject{})
}

func (bikecoachProject) Name() string {
	return "bikecoach"
}

func (bikecoachProject) DefaultTarget() string {
	return "up"
}

func (bikecoachProject) DetectWorktree(worktree string) bool {
	required := []string{
		"sqlc.yaml",
		"cmd/coach/main.go",
		"frontend/package.json",
		"frontend-admin/package.json",
	}
	for _, rel := range required {
		if _, err := os.Stat(filepath.Join(worktree, rel)); err != nil {
			return false
		}
	}
	return true
}

func (bikecoachProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx
	dotenv, err := project.LoadOptionalDotEnvInWorktree(worktree, ".env")
	if err != nil {
		return project.InstanceConfig{}, err
	}
	dotenv = normalizeBikecoachEnv(dotenv)
	manager := database.New()

	return project.InstanceConfig{
		Label:     filepath.Base(worktree),
		PortNames: []string{"backend", "postgres"},
		Env: project.MergeEnvMaps(map[string]string{
			"ENVIRONMENT":            "development",
			"DEV_AUTH_BYPASS":        "1",
			"ADMIN_PORTAL_PASSWORD":  "devflow-admin",
			"DEVFLOW_ADAPTER":        "bikecoach",
			"DEVFLOW_ADAPTER_SOURCE": "examples/bikecoach",
		}, dotenv),
		Finalize: func(inst *api.Instance) error {
			db := manager.Desired(inst.ID, database.Config{
				HostPort:     inst.Ports["postgres"],
				Database:     "coach",
				User:         "coach",
				Password:     "coach",
				SnapshotRoot: filepath.Join(inst.Worktree, ".devflow", "dbsnapshots", "bikecoach"),
			})
			inst.DB = db
			inst.Env = project.MergeEnvMaps(inst.Env, map[string]string{
				"PORT":                strconv.Itoa(inst.Ports["backend"]),
				"PGHOST":              db.Host,
				"PGPORT":              strconv.Itoa(db.Port),
				"PGDATABASE":          db.Name,
				"PGUSER":              db.User,
				"PGPASSWORD":          db.Password,
				"DATABASE_URL":        db.URL,
				"STRAVA_REDIRECT_URI": fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", inst.Ports["backend"]),
				"ENVIRONMENT":         firstNonEmpty(inst.Env["ENVIRONMENT"], "development"),
			})
			return nil
		},
	}, nil
}

func (bikecoachProject) Tasks() []project.Task {
	toolsBin := bikecoachBinaryTool(
		"build_tools",
		"Build the BikeCoach tools CLI",
		".devflow/bikecoach/bin/tools",
		[]string{"check_build_tools", "warmup_go_download", "sqlc_generate"},
		[]string{"go.mod", "go.sum"},
		[]string{"cmd/tools", "internal"},
		process.CommandSpec{
			Name: "go",
			Args: []string{"build", "-o", ".devflow/bikecoach/bin/tools" + exeSuffix(), "./cmd/tools"},
		},
	)
	coachBin := bikecoachBinaryTool(
		"build_coach",
		"Build the BikeCoach server binary",
		".devflow/bikecoach/bin/coach",
		[]string{"check_build_tools", "warmup_go_download", "sqlc_generate", "build_frontend_main", "build_frontend_internal", "build_frontend_admin"},
		[]string{"go.mod", "go.sum"},
		[]string{"cmd/coach", "internal"},
		process.CommandSpec{
			Name: "go",
			Args: []string{"build", "-o", ".devflow/bikecoach/bin/coach" + exeSuffix(), "./cmd/coach"},
		},
	)

	tasks := []project.Task{
		{
			Name:        "check_build_tools",
			Kind:        project.KindOnce,
			Description: "Verify the build toolchain required for BikeCoach",
			Signature:   "bikecoach-check-build-tools-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				for _, cmd := range []string{"go", "npm", "sqlc"} {
					if err := project.EnsureCommandExists(cmd); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			Name:        "check_db_tools",
			Kind:        project.KindOnce,
			Description: "Verify the database runtime tooling required for BikeCoach",
			Signature:   "bikecoach-check-db-tools-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				if err := project.EnsureCommandExists("docker"); err != nil {
					return err
				}
				cmd := exec.Command("docker", "info")
				cmd.Dir = rt.Worktree
				if out, err := cmd.CombinedOutput(); err != nil {
					text := strings.TrimSpace(string(out))
					if text == "" {
						text = err.Error()
					}
					return fmt.Errorf("docker daemon not ready: %s", text)
				}
				return nil
			},
		},
		{
			Name:        "warmup_go_download",
			Kind:        project.KindWarmup,
			Deps:        []string{"check_build_tools"},
			Description: "Warm Go module downloads",
			Signature:   "bikecoach-go-download-v1",
			Inputs:      project.Inputs{Files: []string{"go.mod", "go.sum"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				return rt.RunCmd(ctx, "go", "mod", "download")
			},
		},
		bikecoachFrontendBuildTask("build_frontend_main", "frontend", "internal/web/frontend"),
		bikecoachFrontendBuildTask("build_frontend_internal", "frontend-internal", "internal/web/internal_frontend"),
		bikecoachFrontendBuildTask("build_frontend_admin", "frontend-admin", "internal/web/admin_frontend"),
		{
			Name:        "frontend_assets",
			Kind:        project.KindGroup,
			Deps:        []string{"build_frontend_main", "build_frontend_internal", "build_frontend_admin"},
			Description: "Aggregate BikeCoach embedded frontend builds",
		},
		{
			Name:        "sqlc_generate",
			Kind:        project.KindOnce,
			Deps:        []string{"check_build_tools"},
			Cache:       true,
			Description: "Generate sqlc storage bindings",
			Signature:   "bikecoach-sqlc-generate-v1",
			Inputs: project.Inputs{
				Files: []string{"sqlc.yaml"},
				Dirs:  []string{"internal/storage/queries", "internal/storage/migrations"},
			},
			Outputs: project.Outputs{Dirs: []string{"internal/storage/sqlc"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				return rt.RunCmd(ctx, "sqlc", "generate")
			},
		},
		toolsBin.BuildTask(),
		coachBin.BuildTask(),
		{
			Name:        "prepare_db_base",
			Kind:        project.KindOnce,
			Deps:        []string{"check_db_tools"},
			Description: "Restore the nearest cached database snapshot or reset the Postgres volume",
			Signature:   "bikecoach-prepare-db-base-v1",
			Inputs:      project.Inputs{Dirs: []string{"internal/storage/migrations"}, Files: []string{"sqlc.yaml"}},
			Outputs:     project.Outputs{Files: []string{".devflow/bikecoach/db/prepare.json"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				state, err := inspectBikecoachDBState(rt.Worktree)
				if err != nil {
					return err
				}
				manager := database.New()
				var restored *database.PrismaRestoreResult
				if bikecoachUseFakeDB() {
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
				return writeJSONFile(rt, ".devflow/bikecoach/db/prepare.json", map[string]any{
					"database":     rt.Instance.DB.Name,
					"snapshotKey":  snapshotKey(restored),
					"restored":     restored != nil,
					"exactMatch":   exactMatch(restored),
					"prefixLength": prefixLength(restored),
					"state":        state,
				})
			},
		},
		{
			Name:        "prepare_db_runtime",
			Kind:        project.KindOnce,
			Deps:        []string{"prepare_db_base"},
			Description: "Start the temporary Postgres runtime used during DB preparation",
			Signature:   "bikecoach-prepare-db-runtime-v1",
			Inputs:      project.Inputs{Files: []string{".devflow/bikecoach/db/prepare.json"}},
			Outputs:     project.Outputs{Files: []string{".devflow/bikecoach/db/runtime.json"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				if bikecoachUseFakeDB() {
					return writeJSONFile(rt, ".devflow/bikecoach/db/runtime.json", map[string]any{
						"fake":      true,
						"container": rt.Instance.DB.ContainerName,
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
				return writeJSONFile(rt, ".devflow/bikecoach/db/runtime.json", map[string]any{
					"fake":      false,
					"container": rt.Instance.DB.ContainerName,
					"port":      rt.Instance.DB.Port,
				})
			},
		},
		{
			Name:        "db_migrate",
			Kind:        project.KindOnce,
			Deps:        []string{"prepare_db_runtime", "build_tools"},
			Description: "Run BikeCoach database migrations against the prepared Postgres runtime",
			Signature:   "bikecoach-db-migrate-v1",
			Inputs:      project.Inputs{Dirs: []string{"internal/storage/migrations"}, Files: []string{".devflow/bikecoach/db/prepare.json"}},
			Outputs:     project.Outputs{Files: []string{".devflow/bikecoach/db/migrate.json"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				prepare, err := loadJSONMap(rt, ".devflow/bikecoach/db/prepare.json")
				if err != nil {
					return err
				}
				if !bikecoachUseFakeDB() {
					if err := toolsBin.Run(ctx, rt, "migrate"); err != nil {
						return err
					}
				}
				return writeJSONFile(rt, ".devflow/bikecoach/db/migrate.json", map[string]any{
					"database":     rt.Instance.DB.Name,
					"snapshotKey":  prepare["snapshotKey"],
					"restored":     prepare["restored"],
					"exactMatch":   prepare["exactMatch"],
					"prefixLength": prepare["prefixLength"],
				})
			},
		},
		{
			Name:        "snapshot_db_state",
			Kind:        project.KindOnce,
			Deps:        []string{"db_migrate"},
			Description: "Snapshot the prepared Postgres volume for future restore",
			Signature:   "bikecoach-snapshot-db-state-v1",
			Inputs:      project.Inputs{Dirs: []string{"internal/storage/migrations"}, Files: []string{"sqlc.yaml", ".devflow/bikecoach/db/migrate.json"}},
			Outputs:     project.Outputs{Files: []string{".devflow/bikecoach/db/snapshot.json"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				state, err := inspectBikecoachDBState(rt.Worktree)
				if err != nil {
					return err
				}
				prepare, err := loadJSONMap(rt, ".devflow/bikecoach/db/prepare.json")
				if err != nil {
					return err
				}
				reused := false
				if exact, _ := prepare["exactMatch"].(bool); exact {
					if restored, _ := prepare["restored"].(bool); restored {
						reused = true
					}
				}
				if bikecoachUseFakeDB() {
					if !reused {
						if _, err := database.SavePrismaSnapshot(rt.Instance.DB.SnapshotRoot, state.FullHash, state); err != nil {
							return err
						}
					}
					return writeJSONFile(rt, ".devflow/bikecoach/db/snapshot.json", map[string]any{
						"key":    state.FullHash,
						"reused": reused,
						"fake":   true,
					})
				}
				if reused {
					return writeJSONFile(rt, ".devflow/bikecoach/db/snapshot.json", map[string]any{
						"key":    state.FullHash,
						"reused": true,
						"fake":   false,
					})
				}
				result, err := database.New().SnapshotPrisma(ctx, rt.Instance.DB, state.FullHash, state)
				if err != nil {
					return err
				}
				return writeJSONFile(rt, ".devflow/bikecoach/db/snapshot.json", map[string]any{
					"key":    result.Plan.SnapshotKey,
					"reused": false,
					"fake":   false,
				})
			},
		},
		{
			Name:         "postgres",
			Kind:         project.KindService,
			Deps:         []string{"snapshot_db_state"},
			Restart:      project.RestartOnInputChange,
			Description:  "Run the dedicated Postgres runtime for this BikeCoach worktree",
			Signature:    "bikecoach-postgres-runtime-v1",
			Ready:        bikecoachDBReady,
			ReadyTimeout: 30 * time.Second,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				if bikecoachUseFakeDB() {
					readyPath := rt.Abs(".devflow/bikecoach/runtime/postgres.ready")
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
			Name:        "build_all",
			Kind:        project.KindGroup,
			Deps:        []string{"frontend_assets", "build_tools", "build_coach"},
			Description: "Aggregate BikeCoach build target",
		},
		{
			Name:                      "backend_dev",
			Kind:                      project.KindService,
			Deps:                      []string{"build_all", "postgres"},
			Restart:                   project.RestartOnInputChange,
			WatchRestartOnServiceDeps: true,
			Description:               "Run the BikeCoach HTTP server with embedded frontend assets",
			Signature:                 "bikecoach-backend-dev-v1",
			Ready:                     project.ReadyHTTPNamedPort("backend", "/health", 200),
			ReadyTimeout:              30 * time.Second,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_, err := coachBin.StartSpec(ctx, rt, project.BinaryExecSpec{Grace: 10 * time.Second})
				return err
			},
		},
	}
	return tasks
}

func (bikecoachProject) Targets() []project.Target {
	return []project.Target{
		{
			Name:        "build-all",
			RootTasks:   []string{"build_all"},
			Description: "Build BikeCoach frontend assets and Go binaries",
		},
		{
			Name:        "db-only",
			RootTasks:   []string{"postgres"},
			Description: "Prepare and run the dedicated BikeCoach Postgres instance",
		},
		{
			Name:        "fullstack",
			RootTasks:   []string{"backend_dev"},
			Description: "Build and run BikeCoach with a dedicated Postgres instance",
		},
		{
			Name:        "up",
			RootTasks:   []string{"backend_dev"},
			Description: "Alias for the main BikeCoach runtime target",
		},
	}
}

func bikecoachFrontendBuildTask(name, dir, outputDir string) project.Task {
	return project.Task{
		Name:        name,
		Kind:        project.KindOnce,
		Deps:        []string{"check_build_tools"},
		Cache:       true,
		Description: "Build " + dir + " into embedded frontend assets",
		Signature:   "bikecoach-frontend-build-v1:" + dir,
		Inputs: project.Inputs{
			Files: []string{
				filepath.Join(dir, "package.json"),
				filepath.Join(dir, "package-lock.json"),
				filepath.Join(dir, "tsconfig.json"),
				filepath.Join(dir, "vite.config.ts"),
				filepath.Join(dir, "index.html"),
			},
			Dirs:   []string{filepath.Join(dir, "src"), filepath.Join(dir, "public")},
			Ignore: []string{"node_modules", "dist"},
		},
		Outputs: project.Outputs{Dirs: []string{outputDir}},
		Run: func(ctx context.Context, rt *project.Runtime) error {
			install := "install"
			if rt.Mode == api.ModeCI {
				install = "ci"
			}
			if err := rt.RunCmdSpec(ctx, process.CommandSpec{
				Name: "npm",
				Args: []string{install},
				Dir:  rt.Abs(dir),
			}); err != nil {
				return err
			}
			return rt.RunCmdSpec(ctx, process.CommandSpec{
				Name: "npm",
				Args: []string{"run", "build"},
				Dir:  rt.Abs(dir),
			})
		},
	}
}

func bikecoachBinaryTool(taskName, description, output string, deps, files, dirs []string, build process.CommandSpec) project.BinaryTool {
	return project.BinaryTool{
		TaskName:    taskName,
		Description: description,
		Deps:        deps,
		Inputs: project.Inputs{
			Files: files,
			Dirs:  dirs,
			Ignore: []string{
				".git",
				".devflow",
			},
		},
		Output:    output + exeSuffix(),
		Build:     build,
		Signature: description,
	}
}

func inspectBikecoachDBState(worktree string) (*database.PrismaState, error) {
	extra := []string{}
	if _, err := os.Stat(filepath.Join(worktree, "database_seed.sql")); err == nil {
		extra = append(extra, "database_seed.sql")
	}
	return database.InspectPrismaState(worktree, "internal/storage/migrations", "internal/storage/migrations", extra)
}

func loadJSONMap(rt *project.Runtime, rel string) (map[string]any, error) {
	data, err := os.ReadFile(rt.Abs(rel))
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func writeJSONFile(rt *project.Runtime, rel string, payload any) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return project.WriteFile(rt, rel, append(data, '\n'), 0o644)
}

func normalizeBikecoachEnv(dotenv map[string]string) map[string]string {
	if len(dotenv) == 0 {
		return dotenv
	}
	out := cloneEnv(dotenv)
	if out["ENABLE_COACH_CRON"] == "" && out["EnableCoachCron"] != "" {
		out["ENABLE_COACH_CRON"] = out["EnableCoachCron"]
	}
	return out
}

func bikecoachUseFakeDB() bool {
	return strings.TrimSpace(os.Getenv("DEVFLOW_BIKECOACH_FAKE_DB")) == "1"
}

func bikecoachDBReady(ctx context.Context, rt *project.Runtime) error {
	if bikecoachUseFakeDB() {
		return project.ReadyFile(".devflow/bikecoach/runtime/postgres.ready")(ctx, rt)
	}
	return database.New().WaitReady(ctx, rt.Instance.DB, 30*time.Second)
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func shellQuote(value string) string {
	value = strings.ReplaceAll(value, `'`, `'"'"'`)
	return "'" + value + "'"
}

func cloneEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func snapshotKey(restored *database.PrismaRestoreResult) string {
	if restored == nil {
		return ""
	}
	return restored.Plan.SnapshotKey
}

func exactMatch(restored *database.PrismaRestoreResult) bool {
	return restored != nil && restored.Plan.ExactMatch
}

func prefixLength(restored *database.PrismaRestoreResult) int {
	if restored == nil {
		return 0
	}
	return restored.Plan.PrefixLength
}

func SeedWorktree(dst string) error {
	files := map[string]string{
		".env":              "DATABASE_URL=postgres://coach:coach@localhost:5432/coach?sslmode=disable\nSTRAVA_CLIENT_ID=test-client\nSTRAVA_CLIENT_SECRET=test-secret\nSTRAVA_REDIRECT_URI=http://localhost:8080/oauth/callback\n",
		"go.mod":            "module github.com/example/bikecoach\n\ngo 1.23.0\n",
		"go.sum":            "",
		"sqlc.yaml":         "version: \"2\"\n",
		"cmd/coach/main.go": "package main\nfunc main() {}\n",
		"cmd/tools/main.go": "package main\nfunc main() {}\n",
		"internal/storage/migrations/001_init.up.sql":   "create table widgets(id serial primary key);\n",
		"internal/storage/migrations/001_init.down.sql": "drop table widgets;\n",
		"internal/storage/queries/widgets.sql":          "-- name: ListWidgets :many\nselect 1;\n",
		"frontend/package.json":                         "{\n  \"name\": \"frontend\",\n  \"scripts\": {\"build\": \"echo main\"}\n}\n",
		"frontend/package-lock.json":                    "{}\n",
		"frontend/tsconfig.json":                        "{}\n",
		"frontend/vite.config.ts":                       "export default {}\n",
		"frontend/index.html":                           "<html></html>\n",
		"frontend/src/main.tsx":                         "console.log('main')\n",
		"frontend-internal/package.json":                "{\n  \"name\": \"frontend-internal\",\n  \"scripts\": {\"build\": \"echo internal\"}\n}\n",
		"frontend-internal/package-lock.json":           "{}\n",
		"frontend-internal/tsconfig.json":               "{}\n",
		"frontend-internal/vite.config.ts":              "export default {}\n",
		"frontend-internal/index.html":                  "<html></html>\n",
		"frontend-internal/src/main.tsx":                "console.log('internal')\n",
		"frontend-admin/package.json":                   "{\n  \"name\": \"frontend-admin\",\n  \"scripts\": {\"build\": \"echo admin\"}\n}\n",
		"frontend-admin/package-lock.json":              "{}\n",
		"frontend-admin/tsconfig.json":                  "{}\n",
		"frontend-admin/vite.config.ts":                 "export default {}\n",
		"frontend-admin/index.html":                     "<html></html>\n",
		"frontend-admin/src/main.tsx":                   "console.log('admin')\n",
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
