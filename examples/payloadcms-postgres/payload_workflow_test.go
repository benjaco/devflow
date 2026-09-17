package payloadcmspostgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/database"
	"github.com/benjaco/devflow/pkg/engine"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

// This exercises unmodified Payload, Drizzle and Next against two disposable
// Postgres databases: development push and migration replay never share a DB.
func TestPayloadWorkflowE2E(t *testing.T) {
	if os.Getenv("DEVFLOW_E2E_PAYLOAD") != "1" {
		t.Skip("set DEVFLOW_E2E_PAYLOAD=1 with Node/npm and Docker available")
	}
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("testdata/workflow")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if _, err := process.Run(ctx, process.CommandSpec{Name: "npm", Args: []string{"ci", "--no-audit", "--no-fund"}, Dir: root, LogPath: filepath.Join(root, "install.log")}); err != nil {
		data, _ := os.ReadFile(filepath.Join(root, "install.log"))
		t.Fatalf("npm ci: %v\n%s", err, data)
	}
	isolatePayloadUserCache(t)
	mgr := database.New()
	db := mgr.Desired(fmt.Sprintf("payload-workflow-%d", time.Now().UnixNano()), database.Config{HostPort: payloadFreePort(t), Database: "payload_dev", User: "devflow", Password: "devflow"})
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := mgr.DestroyRuntime(cleanup, db, true); err != nil {
			t.Error(err)
		}
	})
	if err := mgr.EnsureRuntime(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := mgr.WaitReady(ctx, db, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ExecSQL(ctx, db, "CREATE DATABASE payload_replay;"); err != nil {
		t.Fatal(err)
	}
	replayDB := db
	replayDB.Name = "payload_replay"
	replayDB.URL = strings.Replace(db.URL, "/payload_dev", "/payload_replay", 1)
	if err := os.Mkdir(filepath.Join(root, "migrations"), 0700); err != nil {
		t.Fatal(err)
	}
	writePayloadFile(t, root, "schema.json", `{"title":"title","block":"hero","caption":"caption","legacy":true}`)
	p := payloadWorkflowProject(db.URL)
	acceptRename := func(prompt api.Prompt) api.PromptAnswer {
		if prompt.Kind == "select" {
			index := 1
			return api.PromptAnswer{Choice: &index}
		}
		yes := true
		return api.PromptAnswer{Confirm: &yes}
	}
	decline := func(prompt api.Prompt) api.PromptAnswer { no := false; return api.PromptAnswer{Confirm: &no} }

	t.Log("boot real Next/Payload, push collections and blocks, seed data")
	if _, prompts, err := runPayloadWorkflow(t, ctx, root, p, "up", "", api.HeadlessWait, acceptRename); err != nil || len(prompts) != 0 {
		t.Fatalf("initial development: prompts=%+v err=%v", prompts, err)
	}
	if _, err := mgr.ExecSQL(ctx, db, `INSERT INTO posts (title,legacy) VALUES ('keep title','keep legacy'); INSERT INTO posts_blocks_hero (_order,_parent_id,_path,id,caption) VALUES (0,1,'layout','block-1','keep caption');`); err != nil {
		t.Fatal(err)
	}
	if _, prompts, err := runPayloadWorkflow(t, ctx, root, p, "payload_new_migration", "initial", api.HeadlessWait, acceptRename); err != nil || len(prompts) != 0 {
		t.Fatalf("initial authoring: %+v %v", prompts, err)
	}
	if _, _, err := runPayloadWorkflow(t, ctx, root, payloadWorkflowProject(replayDB.URL), "migrate", "", api.HeadlessFail, nil); err != nil {
		t.Fatalf("replay initial: %v", err)
	}
	if _, err := mgr.ExecSQL(ctx, replayDB, `INSERT INTO posts (title,legacy) VALUES ('replay title','replay legacy'); INSERT INTO posts_blocks_hero (_order,_parent_id,_path,id,caption) VALUES (0,1,'layout','block-1','replay caption');`); err != nil {
		t.Fatal(err)
	}

	t.Log("edit field, block slug and block field; answer live development menus")
	watchPayloadEdit(t, ctx, root, p, func() {
		writePayloadFile(t, root, "schema.json", `{"title":"headline","block":"hero_banner","caption":"heading","legacy":true}`)
	}, acceptRename)
	assertPayloadSQL(t, ctx, mgr, db, `SELECT headline,legacy FROM posts; SELECT heading FROM posts_blocks_hero_banner;`, "keep title", "keep legacy", "keep caption")
	t.Log("author both migration directions and replay against previously migrated data")
	if _, prompts, err := runPayloadWorkflow(t, ctx, root, p, "payload_new_migration", "rename", api.HeadlessWait, acceptRename); err != nil || len(prompts) != 6 {
		t.Fatalf("rename authoring: prompts=%+v err=%v", prompts, err)
	}
	assertPayloadMigration(t, root, "rename", `RENAME TO "posts_blocks_hero_banner"`, `RENAME COLUMN "title" TO "headline"`, `RENAME COLUMN "headline" TO "title"`)
	if _, _, err := runPayloadWorkflow(t, ctx, root, payloadWorkflowProject(replayDB.URL), "migrate", "", api.HeadlessFail, nil); err != nil {
		t.Fatalf("replay rename: %v", err)
	}
	assertPayloadSQL(t, ctx, mgr, replayDB, `SELECT headline,legacy FROM posts; SELECT heading FROM posts_blocks_hero_banner;`, "replay title", "replay legacy", "replay caption")
	if _, prompts, err := runPayloadWorkflow(t, ctx, root, p, "up", "", api.HeadlessWait, acceptRename); err != nil || len(prompts) != 0 {
		t.Fatalf("unchanged restart: %+v %v", prompts, err)
	}

	t.Log("choose create instead of rename and inspect different SQL")
	writePayloadFile(t, root, "schema.json", `{"title":"summary","block":"hero_banner","caption":"heading","legacy":true}`)
	create := func(prompt api.Prompt) api.PromptAnswer { index := 0; return api.PromptAnswer{Choice: &index} }
	if _, prompts, err := runPayloadWorkflow(t, ctx, root, p, "payload_new_migration", "create", api.HeadlessWait, create); err != nil || len(prompts) != 2 {
		t.Fatalf("create branch: %+v %v", prompts, err)
	}
	assertPayloadMigration(t, root, "create", `ADD COLUMN "summary"`, `DROP COLUMN "headline"`)
	before, _ := os.ReadDir(filepath.Join(root, "migrations"))
	if _, prompts, err := runPayloadWorkflow(t, ctx, root, p, "payload_new_migration", "declined_blank", api.HeadlessWait, decline); err == nil || len(prompts) != 1 {
		t.Fatalf("blank decline retried/succeeded: %+v %v", prompts, err)
	}
	after, _ := os.ReadDir(filepath.Join(root, "migrations"))
	if len(before) != len(after) {
		t.Fatal("declining a blank migration changed history")
	}
	if _, prompts, err := runPayloadWorkflow(t, ctx, root, p, "payload_new_migration", "accepted_blank", api.HeadlessWait, acceptRename); err != nil || len(prompts) != 1 {
		t.Fatalf("blank acceptance: %+v %v", prompts, err)
	}
	assertPayloadMigration(t, root, "accepted_blank", "// Migration code")

	t.Log("headless and explicit cancellation must terminate, retaining the question")
	writePayloadFile(t, root, "schema.json", `{"title":"another_title","block":"hero_banner","caption":"heading","legacy":true}`)
	for _, policy := range []api.HeadlessPolicy{api.HeadlessFail, api.HeadlessWait} {
		answer := func(api.Prompt) api.PromptAnswer { return api.PromptAnswer{Cancel: true} }
		_, prompts, err := runPayloadWorkflow(t, ctx, root, p, "payload_new_migration", "cancelled", policy, answer)
		var detail *api.CommandError
		want := "interaction_required"
		if policy == api.HeadlessWait {
			want = "interaction_cancelled"
		}
		if !errors.As(err, &detail) || detail.Code != want || len(prompts) != 1 {
			t.Fatalf("prompt stop: prompts=%+v err=%v want=%s", prompts, err, want)
		}
	}

	t.Log("a migration against a pushed DB is answerable and declining preserves data")
	if _, prompts, err := runPayloadWorkflow(t, ctx, root, p, "migrate", "", api.HeadlessWait, decline); err == nil || len(prompts) != 1 {
		t.Fatalf("mixed workflow warning: %+v %v", prompts, err)
	}
	assertPayloadSQL(t, ctx, mgr, db, `SELECT headline,legacy FROM posts;`, "keep title", "keep legacy")

	t.Log("decline destructive dev push, retry, then explicitly accept")
	writePayloadFile(t, root, "schema.json", `{"title":"headline","block":"hero_banner","caption":"heading","legacy":false}`)
	if _, prompts, err := runPayloadWorkflow(t, ctx, root, p, "up", "", api.HeadlessWait, decline); err == nil || len(prompts) != 1 {
		t.Fatalf("destructive push decline: %+v %v", prompts, err)
	}
	assertPayloadSQL(t, ctx, mgr, db, `SELECT legacy FROM posts;`, "keep legacy")
	if _, prompts, err := runPayloadWorkflow(t, ctx, root, p, "up", "", api.HeadlessWait, acceptRename); err != nil || len(prompts) != 1 {
		t.Fatalf("destructive push retry: %+v %v", prompts, err)
	}
	assertPayloadSQL(t, ctx, mgr, db, `SELECT headline FROM posts; SELECT heading FROM posts_blocks_hero_banner;`, "keep title", "keep caption")
}

func watchPayloadEdit(t *testing.T, ctx context.Context, root string, p project.Project, edit func(), answer func(api.Prompt) api.PromptAnswer) {
	t.Helper()
	eng, err := engine.New(p, root)
	if err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEvents()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	done := make(chan error, 1)
	go func() {
		done <- eng.Watch(ctx, engine.Request{Worktree: root, Target: "up", Mode: api.ModeWatch, Headless: api.HeadlessWait})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("watch failed to join")
		}
	}()
	id, _, err := instance.IDForWorktree(root)
	if err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	initial := ""
	count := 0
	for {
		select {
		case <-ctx.Done():
			t.Fatal("watch did not complete the schema edit")
		case <-ticker.C:
			if initial != "" {
				continue
			}
			state, err := instance.LoadStatus(root, id)
			if err != nil || state.Mode != api.ModeWatch || !state.Nodes["app"].Ready || state.Nodes["app"].State != api.StateRunning {
				continue
			}
			initial = state.Nodes["app"].AttemptID
			edit()
		case event := <-events:
			if event.Type == api.EventInteractionReq {
				prompts, err := instance.ListPrompts(ctx, root, id, event.RunID)
				if err != nil {
					t.Fatal(err)
				}
				for _, prompt := range prompts {
					if prompt.ID == event.PromptID {
						count++
						response := answer(prompt)
						response.RunID, response.Task, response.AttemptID, response.PromptID = prompt.RunID, prompt.Task, prompt.AttemptID, prompt.ID
						if err := instance.RespondPrompt(ctx, root, id, response); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if initial != "" && event.Type == api.EventWatchCycleDone && containsStringPayloadTest(event.Files, "schema.json") {
				state, err := instance.LoadStatus(root, id)
				if err != nil {
					t.Fatal(err)
				}
				app := state.Nodes["app"]
				if event.Success == nil || !*event.Success || !app.Ready || app.AttemptID == initial || count < 3 {
					data, _ := os.ReadFile(app.LogPath)
					t.Fatalf("watch edit failed: prompts=%d app=%+v\n%s", count, app, data)
				}
				return
			}
		}
	}
}

func payloadWorkflowProject(databaseURL string) project.Project {
	return project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("payload-workflow")
		b.Env("DATABASE_URL", databaseURL)
		b.Env("NODE_OPTIONS", "--no-deprecation")
		b.Env("NEXT_TELEMETRY_DISABLED", "1")
		p := database.PayloadCMS("payload").Config("payload.config.mjs").MigrationDir("migrations").SchemaInputs("schema.json").Command("node", "node_modules/payload/bin.js")
		app := b.Service("app").Command("node", "node_modules/next/dist/bin/next", "dev", "--hostname", "127.0.0.1").Env("PORT", b.Port("app")).ReadyHTTP("app", "/health", 200).ReadyTimeout(60 * time.Second).RestartOnInputChange()
		p.ConfigureDevService(app)
		p.NewMigration(b)
		b.Target("up", app)
		b.Target("migrate", p.Migrations(b))
		return nil
	})
}

func runPayloadWorkflow(t *testing.T, ctx context.Context, root string, p project.Project, target, name string, policy api.HeadlessPolicy, answer func(api.Prompt) api.PromptAnswer) (*engine.Outcome, []api.Prompt, error) {
	t.Helper()
	p, target, err := project.ResolveExecutionProject(p, target)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(p, root)
	if err != nil {
		t.Fatal(err)
	}
	events := eng.SubscribeEventsLossless()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	t.Setenv("DEVFLOW_MIGRATION_NAME", name)
	type result struct {
		out *engine.Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := eng.Run(ctx, engine.Request{Worktree: root, Target: target, Mode: api.ModeCI, Headless: policy})
		done <- result{out, err}
	}()
	joined := false
	defer func() {
		cancel()
		if joined {
			return
		}
		watchdog := time.NewTimer(10 * time.Second)
		defer watchdog.Stop()
		for {
			select {
			case <-events:
			case <-done:
				return
			case <-watchdog.C:
				t.Error("engine did not join after cancellation")
				return
			}
		}
	}()
	var prompts []api.Prompt
	for {
		select {
		case event := <-events:
			if event.Type != api.EventInteractionReq {
				continue
			}
			listed, err := instance.ListPrompts(ctx, root, event.InstanceID, event.RunID)
			if err != nil {
				t.Fatal(err)
			}
			var prompt api.Prompt
			for _, item := range listed {
				if item.ID == event.PromptID {
					prompt = item
				}
			}
			if prompt.ID == "" {
				t.Fatal("event has no reconnectable prompt")
			}
			prompts = append(prompts, prompt)
			t.Logf("%s: %s (%d choices)", target, prompt.Message, len(prompt.Choices))
			if policy == api.HeadlessFail {
				continue
			}
			response := answer(prompt)
			response.RunID, response.Task, response.AttemptID, response.PromptID = prompt.RunID, prompt.Task, prompt.AttemptID, prompt.ID
			if err := instance.RespondPrompt(ctx, root, event.InstanceID, response); err != nil {
				t.Fatal(err)
			}
		case result := <-done:
			joined = true
			if result.err != nil && result.out != nil {
				data, _ := os.ReadFile(result.out.Result.FailedNodeLogPath)
				t.Logf("failure log: %s", data)
			}
			return result.out, prompts, result.err
		case <-ctx.Done():
			// Drain the lossless subscriber while cancellation joins the engine.
			for {
				select {
				case <-events:
				case result := <-done:
					joined = true
					return result.out, prompts, result.err
				}
			}
		}
	}
}

func payloadFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func assertPayloadSQL(t *testing.T, ctx context.Context, mgr *database.Manager, db api.DBInstance, query string, want ...string) {
	t.Helper()
	data, err := mgr.ExecSQL(ctx, db, query)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range want {
		if !strings.Contains(string(data), text) {
			t.Fatalf("SQL result missing %q: %s", text, data)
		}
	}
}

func assertPayloadMigration(t *testing.T, root, name string, want ...string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "migrations", "*_"+name+".ts"))
	if err != nil || len(files) != 1 {
		t.Fatalf("migration %s: files=%v err=%v", name, files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range want {
		if !strings.Contains(string(data), sql) {
			t.Fatalf("migration lacks %q:\n%s", sql, data)
		}
	}
}
