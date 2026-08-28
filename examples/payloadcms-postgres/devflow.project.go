package payloadcmspostgres

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/benjaco/devflow/pkg/database"
	"github.com/benjaco/devflow/pkg/project"
)

type payloadCMSProject struct {
	project.Project
}

func init() {
	project.Register(payloadCMSProject{Project: buildPayloadCMSProject()})
}

func (payloadCMSProject) DefaultTarget() string {
	return "up"
}

func (payloadCMSProject) DetectWorktree(worktree string) bool {
	required := []string{
		"package.json",
		"src/payload.config.ts",
		"src/collections/Posts.ts",
	}
	for _, rel := range required {
		if _, err := os.Stat(filepath.Join(worktree, rel)); err != nil {
			return false
		}
	}
	return true
}

func buildPayloadCMSProject() project.Project {
	return project.Define(func(ctx context.Context, b *project.Builder) error {
		_ = ctx
		b.Name("payloadcms-postgres")
		b.DefaultTarget("up")
		b.CacheNamespace("payloadcms-postgres")
		b.DotEnv(".env")
		b.RequiredCLIs("npm")
		b.Env("PAYLOAD_SECRET", "devflow-payload-secret")

		db := database.Postgres("payload").PortName("postgres")
		payload := database.PayloadCMS("payload").
			Config("src/payload.config.ts").
			MigrationDir("src/migrations").
			Database(db)

		npmInstall := b.Task("npm_install").
			Command("npm", "install").
			Inputs("package.json", "package-lock.json").
			Stamp()

		migrations := payload.Migrations(b).DependsOn(npmInstall)
		payload.NewMigration(b).DependsOn(npmInstall)

		app := b.Service("app").
			Command("npm", "run", "dev").
			DependsOn(migrations).
			Inputs("src", "package.json", "package-lock.json").
			InputEnv("DATABASE_URL", "PAYLOAD_SECRET", "PORT").
			Env("PORT", b.Port("app")).
			ReadyHTTP("app", "/health", 200).
			ReadyTimeout(30 * time.Second).
			RestartOnInputChange()
		payload.ConfigureDevService(app)

		smoke := b.Task("smoke").
			Command("npm", "run", "smoke").
			DependsOn(migrations).
			Inputs("src/smoke.ts", "package.json", "package-lock.json").
			InputEnv("DATABASE_URL", "PAYLOAD_SECRET").
			NoCache()

		b.Target("setup", npmInstall, migrations)
		b.Target("test", smoke)
		b.Target("up", app)
		return nil
	})
}
