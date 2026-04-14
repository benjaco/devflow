package main

import (
	"context"
	"fmt"
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

type coachProject struct{}

func init() {
	project.Register(coachProject{})
}

func (coachProject) Name() string {
	return "bikecoach"
}

func (coachProject) DefaultTarget() string {
	return "up"
}

func (coachProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	_ = ctx

	dotenv, err := project.LoadOptionalDotEnvInWorktree(worktree, ".env")
	if err != nil {
		return project.InstanceConfig{}, err
	}

	manager := database.New()

	return project.InstanceConfig{
		Label:     filepath.Base(worktree),
		PortNames: []string{"backend", "postgres"},
		Env: project.MergeEnvMaps(map[string]string{
			"ENVIRONMENT": "development",
		}, dotenv),
		Finalize: func(inst *api.Instance) error {
			db := manager.Desired(inst.ID, database.Config{
				HostPort:     inst.Ports["postgres"],
				Database:     "coach",
				User:         "coach",
				Password:     "coach",
				SnapshotRoot: filepath.Join(inst.Worktree, ".devflow", "dbsnapshots", "coach"),
			})
			inst.DB = db
			inst.Env = project.MergeEnvMaps(inst.Env, map[string]string{
				"PORT":         strconv.Itoa(inst.Ports["backend"]),
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

func (coachProject) Dependencies() []project.Dependency {
	return []project.Dependency{
		{
			Name:    "go",
			Command: "go",
			Install: map[string]project.InstallScript{
				"darwin":  {Script: "brew install go"},
				"linux":   {Script: "if command -v apt-get >/dev/null 2>&1; then sudo apt-get update && sudo apt-get install -y golang-go; else echo 'unsupported linux package manager for Go'; exit 1; fi"},
				"windows": {Shell: "powershell", Script: "choco install golang -y"},
			},
		},
		{
			Name:    "npm",
			Command: "npm",
			Install: map[string]project.InstallScript{
				"darwin":  {Script: "brew install node"},
				"linux":   {Script: "if command -v apt-get >/dev/null 2>&1; then sudo apt-get update && sudo apt-get install -y nodejs npm; else echo 'unsupported linux package manager for Node.js'; exit 1; fi"},
				"windows": {Shell: "powershell", Script: "choco install nodejs -y"},
			},
		},
		{
			Name:    "sqlc",
			Command: "sqlc",
			Install: map[string]project.InstallScript{
				"darwin":  {Script: "brew install sqlc"},
				"linux":   {Script: "if command -v go >/dev/null 2>&1; then GOBIN=${HOME}/.local/bin go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest; else echo 'Go is required to install sqlc on linux'; exit 1; fi"},
				"windows": {Shell: "powershell", Script: "if (Get-Command go -ErrorAction SilentlyContinue) { go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest } else { Write-Error 'Go is required to install sqlc on Windows'; exit 1 }"},
			},
		},
		{
			Name:    "docker",
			Command: "docker",
			Install: map[string]project.InstallScript{
				"darwin":  {Script: "brew install --cask docker"},
				"linux":   {Script: "if command -v apt-get >/dev/null 2>&1; then sudo apt-get update && sudo apt-get install -y docker.io; else echo 'unsupported linux package manager for Docker'; exit 1; fi"},
				"windows": {Shell: "powershell", Script: "choco install docker-desktop -y"},
			},
		},
	}
}

func (coachProject) Tasks() []project.Task {
	toolsBin := project.BinaryTool{
		TaskName:    "build_tools",
		Description: "Build tools binary",
		Deps:        []string{"check_build_tools", "warmup_go_download", "sqlc_generate"},
		Inputs: project.Inputs{
			Files: []string{"go.mod", "go.sum"},
			Dirs:  []string{"cmd/tools", "internal"},
			Ignore: []string{
				".git",
				".devflow",
			},
		},
		Output: ".devflow/coach/bin/tools" + exeSuffix(),
		Build: process.CommandSpec{
			Name: "go",
			Args: []string{"build", "-o", ".devflow/coach/bin/tools" + exeSuffix(), "./cmd/tools"},
		},
	}

	coachBin := project.BinaryTool{
		TaskName:    "build_coach",
		Description: "Build coach server binary",
		Deps: []string{
			"check_build_tools",
			"warmup_go_download",
			"sqlc_generate",
			"build_frontend_main",
			"build_frontend_internal",
			"build_frontend_admin",
		},
		Inputs: project.Inputs{
			Files: []string{"go.mod", "go.sum"},
			Dirs:  []string{"cmd/coach", "internal"},
			Ignore: []string{
				".git",
				".devflow",
			},
		},
		Output: ".devflow/coach/bin/coach" + exeSuffix(),
		Build: process.CommandSpec{
			Name: "go",
			Args: []string{"build", "-o", ".devflow/coach/bin/coach" + exeSuffix(), "./cmd/coach"},
		},
	}

	return []project.Task{
		{
			Name:      "check_build_tools",
			Kind:      project.KindOnce,
			Signature: "coach-check-build-tools-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				_ = rt
				return project.EnsureDependencies(coachProject{}.Dependencies(), "go", "npm", "sqlc")
			},
		},
		{
			Name:      "check_db_tools",
			Kind:      project.KindOnce,
			Signature: "coach-check-db-tools-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_ = ctx
				if err := project.EnsureDependencies(coachProject{}.Dependencies(), "docker"); err != nil {
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
			Name:      "warmup_go_download",
			Kind:      project.KindWarmup,
			Deps:      []string{"check_build_tools"},
			Signature: "coach-go-download-v1",
			Inputs:    project.Inputs{Files: []string{"go.mod", "go.sum"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				return rt.RunCmd(ctx, "go", "mod", "download")
			},
		},
		frontendBuildTask("build_frontend_main", "frontend", "internal/web/frontend"),
		frontendBuildTask("build_frontend_internal", "frontend-internal", "internal/web/internal_frontend"),
		frontendBuildTask("build_frontend_admin", "frontend-admin", "internal/web/admin_frontend"),
		{
			Name: "frontend_assets",
			Kind: project.KindGroup,
			Deps: []string{"build_frontend_main", "build_frontend_internal", "build_frontend_admin"},
		},
		{
			Name:      "sqlc_generate",
			Kind:      project.KindOnce,
			Deps:      []string{"check_build_tools"},
			Cache:     true,
			Signature: "coach-sqlc-generate-v1",
			Inputs:    project.Inputs{Files: []string{"sqlc.yaml"}, Dirs: []string{"internal/storage/queries", "internal/storage/migrations"}},
			Outputs:   project.Outputs{Dirs: []string{"internal/storage/sqlc"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				return rt.RunCmd(ctx, "sqlc", "generate")
			},
		},
		toolsBin.BuildTask(),
		coachBin.BuildTask(),
		{
			Name:      "prepare_db_base",
			Kind:      project.KindOnce,
			Deps:      []string{"check_db_tools"},
			Signature: "coach-prepare-db-base-v1",
			Inputs:    project.Inputs{Dirs: []string{"internal/storage/migrations"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				state, err := database.InspectPrismaState(rt.Worktree, "internal/storage/migrations", "internal/storage/migrations", nil)
				if err != nil {
					return err
				}
				_, err = database.New().PreparePrismaBase(ctx, rt.Instance.DB, state, nil, database.PrepareOptions{
					Worktree: rt.Worktree,
					Env:      cloneEnv(rt.Env),
					LogPath:  rt.LogPath,
				})
				return err
			},
		},
		{
			Name:      "prepare_db_runtime",
			Kind:      project.KindOnce,
			Deps:      []string{"prepare_db_base"},
			Signature: "coach-prepare-db-runtime-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				manager := database.New()
				if err := manager.EnsureRuntime(ctx, rt.Instance.DB); err != nil {
					return err
				}
				return manager.WaitReady(ctx, rt.Instance.DB, 30*time.Second)
			},
		},
		{
			Name:      "db_migrate",
			Kind:      project.KindOnce,
			Deps:      []string{"prepare_db_runtime", "build_tools"},
			Signature: "coach-db-migrate-v1",
			Run: func(ctx context.Context, rt *project.Runtime) error {
				return toolsBin.Run(ctx, rt, "migrate")
			},
		},
		{
			Name:      "snapshot_db_state",
			Kind:      project.KindOnce,
			Deps:      []string{"db_migrate"},
			Signature: "coach-snapshot-db-state-v1",
			Inputs:    project.Inputs{Dirs: []string{"internal/storage/migrations"}},
			Run: func(ctx context.Context, rt *project.Runtime) error {
				state, err := database.InspectPrismaState(rt.Worktree, "internal/storage/migrations", "internal/storage/migrations", nil)
				if err != nil {
					return err
				}
				_, err = database.New().SnapshotPrisma(ctx, rt.Instance.DB, state.FullHash, state)
				return err
			},
		},
		{
			Name:    "postgres",
			Kind:    project.KindService,
			Deps:    []string{"snapshot_db_state"},
			Restart: project.RestartOnInputChange,
			Ready: func(ctx context.Context, rt *project.Runtime) error {
				return database.New().WaitReady(ctx, rt.Instance.DB, 30*time.Second)
			},
			ReadyTimeout: 30 * time.Second,
			Run: func(ctx context.Context, rt *project.Runtime) error {
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
			Name: "build_all",
			Kind: project.KindGroup,
			Deps: []string{"frontend_assets", "build_tools", "build_coach"},
		},
		{
			Name:                      "backend_dev",
			Kind:                      project.KindService,
			Deps:                      []string{"build_all", "postgres"},
			Restart:                   project.RestartOnInputChange,
			WatchRestartOnServiceDeps: true,
			Signature:                 "coach-backend-dev-v1",
			Ready:                     project.ReadyHTTPNamedPort("backend", "/health", 200),
			ReadyTimeout:              30 * time.Second,
			Run: func(ctx context.Context, rt *project.Runtime) error {
				_, err := coachBin.StartSpec(ctx, rt, project.BinaryExecSpec{Grace: 10 * time.Second})
				return err
			},
		},
	}
}

func (coachProject) Targets() []project.Target {
	return []project.Target{
		{Name: "build-all", RootTasks: []string{"build_all"}},
		{Name: "db-only", RootTasks: []string{"postgres"}},
		{Name: "fullstack", RootTasks: []string{"backend_dev"}},
		{Name: "up", RootTasks: []string{"backend_dev"}},
	}
}

func frontendBuildTask(name, dir, outputDir string) project.Task {
	return project.Task{
		Name:      name,
		Kind:      project.KindOnce,
		Deps:      []string{"check_build_tools"},
		Cache:     true,
		Signature: "coach-frontend-build-v1:" + dir,
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

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func cloneEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
