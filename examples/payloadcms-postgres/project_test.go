package payloadcmspostgres

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/testutil"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/database"
	"github.com/benjaco/devflow/pkg/engine"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

func TestPayloadCMSProjectDetectionAndGraph(t *testing.T) {
	worktree := seededPayloadWorktree(t)
	p, err := project.Detect(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Name(); got != "payloadcms-postgres" {
		t.Fatalf("unexpected detected project %q", got)
	}
	if got := project.PreferredTarget(p); got != "up" {
		t.Fatalf("unexpected default target %q", got)
	}
	g, err := graph.New(p.Tasks(), p.Targets())
	if err != nil {
		t.Fatal(err)
	}
	closure, err := g.TargetClosure("up")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"npm_install", "payload_migrations", "app"} {
		if !containsStringPayloadTest(closure, want) {
			t.Fatalf("expected up closure to contain %q, got %v", want, closure)
		}
	}
}

func TestPayloadCMSMigrationsApplyWithFakePayload(t *testing.T) {
	isolatePayloadUserCache(t)
	worktree := seededPayloadWorktree(t)
	fakeBin := installFakeNPM(t)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	recordPath := filepath.Join(worktree, ".devflow", "fake-npm-record.txt")
	t.Setenv("DEVFLOW_FAKE_NPM_RECORD", recordPath)

	eng, err := engine.New(testPayloadProjectWithoutManagedDB(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := eng.Run(context.Background(), engine.Request{
		Target:      "setup",
		Worktree:    worktree,
		Mode:        api.ModeCI,
		MaxParallel: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Result.Success {
		t.Fatalf("expected success, got %+v", outcome.Result)
	}
	applied, err := os.ReadFile(filepath.Join(worktree, ".devflow", "payload-test", "applied.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(applied), "1 migrations") {
		t.Fatalf("unexpected applied marker: %s", string(applied))
	}
	record := readFilePayloadTest(t, recordPath)
	if !strings.Contains(record, "npm run payload -- migrate") {
		t.Fatalf("expected payload migrate command, got:\n%s", record)
	}
}

func TestPayloadCMSNewMigrationForDeletedFieldRequiresConfirmation(t *testing.T) {
	isolatePayloadUserCache(t)
	worktree := seededPayloadWorktree(t)
	writePayloadFile(t, worktree, "src/collections/Posts.ts", `export const Posts = {
  slug: 'posts',
  fields: [
    { name: 'title', type: 'text', required: true },
  ],
}
`)
	fakeBin := installFakeNPM(t)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEVFLOW_MIGRATION_NAME", "drop legacy field")
	t.Setenv("DEVFLOW_FAKE_PAYLOAD_REQUIRE_CONFIRM", "deleted-field")
	t.Setenv("DEVFLOW_FAKE_MIGRATION_TIMESTAMP", "20260519010101")
	recordPath := filepath.Join(worktree, ".devflow", "fake-npm-record.txt")
	t.Setenv("DEVFLOW_FAKE_NPM_RECORD", recordPath)

	execProject, target, err := project.ResolveExecutionProject(testPayloadProjectWithoutManagedDB(), "payload_new_migration")
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(execProject, worktree)
	if err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEvents()
	var promptCount atomic.Int32
	go func() {
		for evt := range events {
			if evt.Type != api.EventInteractionReq {
				continue
			}
			promptCount.Add(1)
			if evt.Task != "payload_new_migration" {
				t.Errorf("unexpected prompt task %q", evt.Task)
				return
			}
			if evt.PromptKind != string(process.PromptConfirm) {
				t.Errorf("unexpected prompt kind %q", evt.PromptKind)
				return
			}
			if !strings.Contains(evt.Prompt, "PayloadCMS") {
				t.Errorf("unexpected prompt text %q", evt.Prompt)
				return
			}
			if err := instance.WriteInteractionAnswer(worktree, evt.InstanceID, evt.PromptID, "y"); err != nil {
				t.Errorf("write interaction answer: %v", err)
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	outcome, err := eng.Run(ctx, engine.Request{
		Target:      target,
		Worktree:    worktree,
		Mode:        api.ModeCI,
		MaxParallel: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Result.Success {
		t.Fatalf("expected success, got %+v", outcome.Result)
	}
	if got := promptCount.Load(); got != 1 {
		t.Fatalf("expected one confirmation prompt, got %d", got)
	}
	migrationPath := filepath.Join(worktree, "src", "migrations", "20260519010101_drop_legacy_field.ts")
	if _, err := os.Stat(migrationPath); err != nil {
		t.Fatalf("expected migration file %s: %v", migrationPath, err)
	}
	record := readFilePayloadTest(t, recordPath)
	if !strings.Contains(record, "npm run payload -- migrate:create drop legacy field") {
		t.Fatalf("expected payload migrate:create command, got:\n%s", record)
	}
}

func testPayloadProjectWithoutManagedDB() project.Project {
	return project.Define(func(ctx context.Context, b *project.Builder) error {
		_ = ctx
		b.Name("payloadcms-postgres-test")
		b.CacheNamespace("payloadcms-postgres-test")
		b.Env("DATABASE_URL", "postgres://devflow:devflow@127.0.0.1:5432/payload?sslmode=disable")
		payload := database.PayloadCMS("payload").
			Config("src/payload.config.ts").
			MigrationDir("src/migrations").
			Command("npm", "run", "payload", "--")
		b.Target("setup", payload.Migrations(b))
		payload.NewMigration(b)
		return nil
	})
}

func seededPayloadWorktree(t *testing.T) string {
	t.Helper()
	worktree := t.TempDir()
	writePayloadFile(t, worktree, "package.json", `{
  "scripts": {
    "payload": "payload",
    "dev": "payload dev",
    "smoke": "node src/smoke.ts"
  },
  "dependencies": {
    "payload": "3.84.1",
    "@payloadcms/db-postgres": "3.84.1"
  }
}
`)
	writePayloadFile(t, worktree, "package-lock.json", `{"lockfileVersion": 3}`)
	writePayloadFile(t, worktree, "src/payload.config.ts", `import { buildConfig } from 'payload'
import { postgresAdapter } from '@payloadcms/db-postgres'
import { Posts } from './collections/Posts'

export default buildConfig({
  secret: process.env.PAYLOAD_SECRET || 'devflow-payload-secret',
  collections: [Posts],
  db: postgresAdapter({ pool: { connectionString: process.env.DATABASE_URL } }),
})
`)
	writePayloadFile(t, worktree, "src/collections/Posts.ts", `export const Posts = {
  slug: 'posts',
  fields: [
    { name: 'title', type: 'text', required: true },
    { name: 'legacy', type: 'text' },
  ],
}
`)
	writePayloadFile(t, worktree, "src/migrations/20260519000000_init.ts", `export async function up() {}
export async function down() {}
`)
	writePayloadFile(t, worktree, "src/smoke.ts", `console.log('ok')
`)
	return worktree
}

func writePayloadFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func installFakeNPM(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	source := testutil.BuildTestCommand(t)
	target := filepath.Join(binDir, "npm"+exeSuffixPayloadTest())
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir
}

func isolatePayloadUserCache(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "LocalAppData"))
}

func containsStringPayloadTest(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func readFilePayloadTest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func exeSuffixPayloadTest() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
