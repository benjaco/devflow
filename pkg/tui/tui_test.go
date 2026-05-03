package tui

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/database"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func TestReadLastLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.log")
	content := strings.Join([]string{"one", "two", "three", "four"}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := readLastLines(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lines, ","); got != "three,four" {
		t.Fatalf("unexpected tail lines: %s", got)
	}
}

func TestTaskStatePriorityOrdersRunningThenPending(t *testing.T) {
	nodes := []api.NodeStatus{
		{Name: "done_task", State: api.StateDone},
		{Name: "pending_task", State: api.StatePending},
		{Name: "migration_task", State: api.StateMigrationNeeded},
		{Name: "running_task", State: api.StateRunning},
		{Name: "cached_task", State: api.StateCached},
	}
	sort.Slice(nodes, func(i, j int) bool {
		left := taskStatePriority(nodes[i].State)
		right := taskStatePriority(nodes[j].State)
		if left != right {
			return left < right
		}
		return nodes[i].Name < nodes[j].Name
	})
	got := make([]string, 0, len(nodes))
	for _, node := range nodes {
		got = append(got, node.Name)
	}
	want := []string{"running_task", "pending_task", "migration_task", "cached_task", "done_task"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected node order: got %v want %v", got, want)
	}
}

func TestRenderHeaderIncludesStateSummary(t *testing.T) {
	snap := snapshot{
		instance: &api.Instance{
			ID:       "abc123",
			Worktree: "/tmp/worktree",
			DB: api.DBInstance{
				Name:          "coach",
				Host:          "127.0.0.1",
				Port:          5433,
				ContainerName: "devflow-pg-abc123",
			},
		},
		state: &instance.State{
			Target:    "fullstack",
			Mode:      api.ModeDev,
			UpdatedAt: time.Now().UTC(),
		},
		nodes: []api.NodeStatus{
			{Name: "postgres", State: api.StateRunning},
			{Name: "backend_dev", State: api.StateRunning},
			{Name: "build", State: api.StateCached},
			{Name: "done", State: api.StateDone},
			{Name: "db_prepare", State: api.StateMigrationNeeded},
		},
		supervisor: &api.SupervisorStatus{PID: 55, Alive: true},
		urls:       map[string]string{"backend": "http://127.0.0.1:8080"},
	}
	lines := renderHeader(snap)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"instance",
		"abc123",
		"backend=http://127.0.0.1:8080",
		"RUN=2",
		"CACHE=1",
		"DONE=1",
		"MIGR=1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected header to contain %q, got:\n%s", want, joined)
		}
	}
}

func TestStateBadgeShowsMigrationNeeded(t *testing.T) {
	if got := stateBadge(api.StateMigrationNeeded); got != "MIGR" {
		t.Fatalf("expected migration-needed badge, got %q", got)
	}
}

func TestApplyEventsReportsMigrationNeededState(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	d.applyEvents([]api.Event{{
		Type:  api.EventTaskState,
		Task:  "db_prepare",
		State: api.StateMigrationNeeded,
		Error: "generate a migration first",
	}})
	if got := d.statusMessage; !strings.Contains(got, "needs a migration") || !strings.Contains(got, "db_prepare") {
		t.Fatalf("expected migration-needed status, got %q", got)
	}
}

func TestApplyEventsReportsFailedMigrationNeededMessage(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	d.applyEvents([]api.Event{{
		Type:  api.EventTaskState,
		Task:  "db_prepare",
		State: api.StateFailed,
		Error: "prisma schema changed without a new migration; generate one with GeneratePrismaMigration before preparing the database",
	}})
	if got := d.statusMessage; !strings.Contains(got, "needs a migration") || strings.Contains(got, "failed") {
		t.Fatalf("expected failed migration-needed message to be presented as migration-needed, got %q", got)
	}
}

func TestNormalizeTUIStatePromotesFailedMigrationNeededSnapshot(t *testing.T) {
	node := normalizeTUIState(api.NodeStatus{
		Name:      "db_prepare",
		State:     api.StateFailed,
		LastError: "prisma schema changed without a new migration; generate one with GeneratePrismaMigration before preparing the database",
	}, nil)
	if node.State != api.StateMigrationNeeded {
		t.Fatalf("expected stale failed migration snapshot to render as migration-needed, got %q", node.State)
	}
}

func TestNormalizeTUIStatePromotesFailedDBPrepareWhenPrismaNeedsMigration(t *testing.T) {
	node := normalizeTUIState(api.NodeStatus{
		Name:  "db_prepare",
		State: api.StateFailed,
	}, &database.PrismaDevelopmentStatus{NeedsNewMigration: true})
	if node.State != api.StateMigrationNeeded {
		t.Fatalf("expected failed db_prepare to render as migration-needed when Prisma panel detects drift, got %q", node.State)
	}
}

func TestRenderLogPanelIncludesSelection(t *testing.T) {
	snap := snapshot{
		nodes: []api.NodeStatus{
			{Name: "backend_dev", Kind: "service", State: api.StateRunning, PID: 12},
		},
		logTitle: "backend_dev log",
		logLines: []string{"line one", "line two"},
	}
	lines := renderLogPanel(snap, "backend_dev")
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"selected: backend_dev",
		"kind=service",
		"state=running",
		"line two",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected log panel to contain %q, got:\n%s", want, joined)
		}
	}
}

func TestRenderDatabasePanelIncludesPrismaSnapshots(t *testing.T) {
	snap := snapshot{
		instance: &api.Instance{
			DB: api.DBInstance{
				Name:          "app_wt_abc",
				Host:          "127.0.0.1",
				Port:          55432,
				User:          "devflow",
				Password:      "secret",
				Image:         "postgres:16.3",
				ContainerName: "devflow-pg-abc",
				VolumeName:    "devflow-pgdata-abc",
				SnapshotRoot:  "/tmp/devflow/db-snapshots",
			},
		},
		logTitle: databasePanelTitle,
		prisma: []prismaSnapshotSummary{
			{
				Key:            "prisma_new",
				CreatedAt:      time.Date(2026, 5, 3, 10, 30, 0, 0, time.UTC),
				MigrationNames: []string{"001_init", "002_add_role"},
			},
		},
	}
	lines := renderLogPanel(snap, "")
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"managed postgres",
		"host=127.0.0.1",
		"port=55432",
		"container=devflow-pg-abc",
		"prisma snapshots: 1 cached migration-prefix states",
		"latest=prisma_new migrations=2",
		"002_add_role",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected database panel to contain %q, got:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "secret") {
		t.Fatalf("database panel leaked DB password:\n%s", joined)
	}
}

func TestRenderDatabasePanelFlagsPrismaMigrationDrift(t *testing.T) {
	snap := snapshot{
		instance: &api.Instance{
			DB: api.DBInstance{Name: "app_wt_abc", Host: "127.0.0.1", Port: 55432, SnapshotRoot: "/tmp/snapshots"},
		},
		logTitle: databasePanelTitle,
		prismaCfg: tuiPrismaConfig{
			PrismaConfig: project.PrismaConfig{SchemaPath: "prisma/schema.prisma", MigrationsDir: "prisma/migrations"},
			Source:       "adapter",
			Available:    true,
		},
		prismaDev: &database.PrismaDevelopmentStatus{
			NeedsNewMigration: true,
			Reason:            "schema_changed",
			Message:           "Prisma schema changed without a new migration",
		},
	}
	lines := renderLogPanel(snap, "")
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"migration folder/state: needs new migration",
		"reason=schema_changed",
		"press m to create a Prisma migration (F4 also works)",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected database panel to contain %q, got:\n%s", want, joined)
		}
	}
}

func TestLoadPrismaSnapshotSummariesSortsAndSkipsNonPrismaSnapshots(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "generic_migration_snapshot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "generic_migration_snapshot", "migrations.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SavePrismaSnapshot(root, "prisma_old", &database.PrismaState{
		SchemaHash: "schema-old",
		Migrations: []database.PrismaMigration{
			{Name: "001_init", Hash: "a"},
		},
		FullHash: "old",
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := database.SavePrismaSnapshot(root, "prisma_new", &database.PrismaState{
		SchemaHash: "schema-new",
		Migrations: []database.PrismaMigration{
			{Name: "001_init", Hash: "a"},
			{Name: "002_posts", Hash: "b"},
		},
		FullHash: "new",
	}); err != nil {
		t.Fatal(err)
	}

	items, err := loadPrismaSnapshotSummaries(root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("expected only Prisma snapshots, got %+v", items)
	}
	if items[0].Key != "prisma_new" {
		t.Fatalf("expected newest Prisma snapshot first, got %+v", items)
	}
	if strings.Join(items[0].MigrationNames, ",") != "001_init,002_posts" {
		t.Fatalf("unexpected migration names: %+v", items[0].MigrationNames)
	}
}

func TestGeneratePrismaMigrationFromTUIRunsDefaultCreateOnly(t *testing.T) {
	worktree := t.TempDir()
	mustWriteTUITestFile(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id name String }\n")
	binDir := filepath.Join(worktree, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteTUITestExecutable(t, filepath.Join(binDir, "npx"), "#!/bin/sh\nprintf '%s' \"$*\" > \"$OUT_FILE\"\nprintf '%s' \"$DATABASE_URL\" > \"$OUT_FILE.url\"\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(worktree, "prisma-command.txt")
	inst.Env = map[string]string{"OUT_FILE": output}
	inst.DB = api.DBInstance{URL: "postgres://devflow:devflow@127.0.0.1:55432/app?sslmode=disable"}
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}

	if err := generatePrismaMigrationFromTUI(worktree, inst.ID, "add-user-name"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(data))
	want := "prisma migrate dev --name add-user-name --schema prisma/schema.prisma --create-only"
	if got != want {
		t.Fatalf("unexpected Prisma migration command: got %q want %q", got, want)
	}
	url, err := os.ReadFile(output + ".url")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(url)) == "" {
		t.Fatal("expected DATABASE_URL to be supplied to Prisma migration command")
	}
}

func TestGeneratePrismaMigrationFromTUIReconcilesManagedDatabase(t *testing.T) {
	worktree := t.TempDir()
	mustWriteTUITestFile(t, filepath.Join(worktree, "prisma", "schema.prisma"), "datasource db {}\nmodel User { id Int @id name String }\n")
	mustWriteTUITestFile(t, filepath.Join(worktree, "prisma", "migrations", "001_init", "migration.sql"), "create table users(id int primary key);\n")
	binDir := filepath.Join(worktree, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteTUITestExecutable(t, filepath.Join(binDir, "npx"), "#!/bin/sh\nprintf ok > \"$OUT_FILE\"\nprintf 'migration created\\n'\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	manager := &recordingTUIDatabaseManager{}
	previousManager := newDatabaseManagerForTUI
	newDatabaseManagerForTUI = func() tuiDatabaseManager { return manager }
	t.Cleanup(func() { newDatabaseManagerForTUI = previousManager })

	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(worktree, "prisma-command.txt")
	inst.Env = map[string]string{"OUT_FILE": output}
	inst.DB = api.DBInstance{
		Name:          "app",
		URL:           "postgres://devflow:devflow@127.0.0.1:55432/app?sslmode=disable",
		Host:          "127.0.0.1",
		Port:          55432,
		User:          "devflow",
		Password:      "devflow",
		ContainerName: "devflow-pg-test",
		VolumeName:    "devflow-pgdata-test",
	}
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}

	var progress []string
	if err := generatePrismaMigrationFromTUI(worktree, inst.ID, "add-age", func(message string) {
		progress = append(progress, message)
	}); err != nil {
		t.Fatal(err)
	}
	if !manager.prepareCalled {
		t.Fatal("expected TUI migration generation to reconcile the managed database")
	}
	if manager.prepareOpts.DB.ContainerName != inst.DB.ContainerName {
		t.Fatalf("expected authoring prep to receive instance database, got %+v", manager.prepareOpts.DB)
	}
	if manager.prepareOpts.Worktree != worktree || manager.prepareOpts.SchemaPath != "prisma/schema.prisma" || manager.prepareOpts.MigrationsDir != "prisma/migrations" {
		t.Fatalf("unexpected authoring prep options: %+v", manager.prepareOpts)
	}
	if _, err := os.ReadFile(output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"loading Prisma migration configuration",
		"reconciling Prisma migration database",
		"Prisma database stdout: database prepared",
		"managed database is ready",
		"running Prisma migration generation",
		"Prisma stdout: migration created",
	} {
		if !tuiProgressContains(progress, want) {
			t.Fatalf("expected progress to contain %q, got %+v", want, progress)
		}
	}
}

func TestDownstreamInvalidateTasksOnlyReturnsCacheableOnceTasksInTargetClosure(t *testing.T) {
	g, err := graph.New([]project.Task{
		{Name: "a", Kind: project.KindOnce, Cache: true},
		{Name: "b", Kind: project.KindOnce, Cache: true, Deps: []string{"a"}},
		{Name: "c", Kind: project.KindService, Deps: []string{"b"}},
		{Name: "d", Kind: project.KindOnce, Cache: false, Deps: []string{"b"}},
		{Name: "e", Kind: project.KindOnce, Cache: true, Deps: []string{"d"}},
	}, []project.Target{
		{Name: "main", RootTasks: []string{"c", "e"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	names, err := downstreamInvalidateTasks(g, "main", "b")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names, ",")
	want := "b,e"
	if got != want {
		t.Fatalf("unexpected invalidate tasks: got %q want %q", got, want)
	}
}

func TestDownstreamInvalidateTasksForGroupReturnsItsCacheableInputs(t *testing.T) {
	g, err := graph.New([]project.Task{
		{Name: "build_a", Kind: project.KindOnce, Cache: true},
		{Name: "build_b", Kind: project.KindOnce, Cache: true},
		{Name: "aggregate", Kind: project.KindGroup, Deps: []string{"build_a", "build_b"}},
		{Name: "serve", Kind: project.KindService, Deps: []string{"aggregate"}},
	}, []project.Target{
		{Name: "main", RootTasks: []string{"serve"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	names, err := downstreamInvalidateTasks(g, "main", "aggregate")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names, ",")
	want := "build_a,build_b"
	if got != want {
		t.Fatalf("unexpected invalidate tasks for group: got %q want %q", got, want)
	}
}

func TestExecutionGraphResolvesTaskTargets(t *testing.T) {
	const name = "tui-execution-graph"
	project.Register(testProject{
		name: name,
		tasks: []project.Task{
			{Name: "build", Kind: project.KindOnce, Cache: true},
			{Name: "serve", Kind: project.KindService, Deps: []string{"build"}},
		},
		targets: []project.Target{
			{Name: "fullstack", RootTasks: []string{"serve"}},
		},
	})
	g, target, err := executionGraph(name, "build")
	if err != nil {
		t.Fatal(err)
	}
	if target != "build" {
		t.Fatalf("expected resolved synthetic target to be build, got %q", target)
	}
	closure, err := g.TargetClosure(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(closure, ","); got != "build" {
		t.Fatalf("unexpected synthetic target closure: %q", got)
	}
}

func TestResolveRelaunchProjectFallsBackToDetectedProject(t *testing.T) {
	worktree := t.TempDir()
	const marker = "detected.txt"
	if err := os.WriteFile(filepath.Join(worktree, marker), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	const name = "tui-relaunch-detected-project"
	project.Register(detectorTestProject{
		testProject: testProject{
			name: name,
			tasks: []project.Task{
				{Name: "build", Kind: project.KindOnce, Cache: true},
			},
			targets: []project.Target{
				{Name: "up", RootTasks: []string{"build"}},
			},
		},
		marker: marker,
	})

	inst := &api.Instance{}
	inst.LastRun.Project = "stale-project-name"

	gotName, gotProject, err := resolveRelaunchProject(worktree, inst)
	if err != nil {
		t.Fatal(err)
	}
	if gotName != name {
		t.Fatalf("unexpected project name: got %q want %q", gotName, name)
	}
	if gotProject.Name() != name {
		t.Fatalf("unexpected project: got %q want %q", gotProject.Name(), name)
	}
}

func TestWriteInvalidateTransitionMarksDirtyAndPendingNodes(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]api.NodeStatus{
		"build_a":   {Name: "build_a", Kind: "once", State: api.StateCached, LastRunKey: "a"},
		"build_b":   {Name: "build_b", Kind: "once", State: api.StateCached, LastRunKey: "b"},
		"aggregate": {Name: "aggregate", Kind: "group", State: api.StateDone},
		"serve":     {Name: "serve", Kind: "service", State: api.StateRunning, PID: 123},
	}
	if err := instance.SaveStatus(worktree, inst.ID, "main", api.ModeDev, nodes); err != nil {
		t.Fatal(err)
	}
	g, err := graph.New([]project.Task{
		{Name: "build_a", Kind: project.KindOnce, Cache: true},
		{Name: "build_b", Kind: project.KindOnce, Cache: true},
		{Name: "aggregate", Kind: project.KindGroup, Deps: []string{"build_a", "build_b"}},
		{Name: "serve", Kind: project.KindService, Deps: []string{"aggregate"}},
	}, []project.Target{{Name: "main", RootTasks: []string{"serve"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeInvalidateTransition(worktree, inst.ID, "main", g, []string{"build_a", "build_b"}); err != nil {
		t.Fatal(err)
	}
	state, err := instance.LoadStatus(worktree, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Nodes["build_a"].State != api.StateDirty || state.Nodes["build_a"].LastRunKey != "" {
		t.Fatalf("expected build_a to become dirty without key, got %+v", state.Nodes["build_a"])
	}
	if state.Nodes["aggregate"].State != api.StatePending {
		t.Fatalf("expected aggregate to become pending, got %+v", state.Nodes["aggregate"])
	}
	if state.Nodes["serve"].State != api.StatePending || state.Nodes["serve"].PID != 0 {
		t.Fatalf("expected serve to become pending without pid, got %+v", state.Nodes["serve"])
	}
}

func TestRenderFooterIncludesRetargetKey(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	d.setStatus("[green]ready")
	text := d.footer.GetText(false)
	if !strings.Contains(text, "t retarget") {
		t.Fatalf("expected footer to advertise retarget key, got %q", text)
	}
	if !strings.Contains(text, "d db") {
		t.Fatalf("expected footer to advertise database panel key, got %q", text)
	}
	if !strings.Contains(text, "m migration") {
		t.Fatalf("expected footer to advertise migration key, got %q", text)
	}
	if !strings.Contains(text, "j/k/arrows move") {
		t.Fatalf("expected footer to advertise normal movement keys, got %q", text)
	}
}

func TestRenderFooterShowsPrismaMigrationProgressStatus(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	d.setStatus(`[yellow]running Prisma migration generation for "add-age"...`)
	text := d.footer.GetText(false)
	if !strings.Contains(text, "m migration") {
		t.Fatalf("expected footer to keep migration shortcut while showing progress, got %q", text)
	}
	if !strings.Contains(text, `running Prisma migration generation for "add-age"`) {
		t.Fatalf("expected footer to render Prisma migration progress, got %q", text)
	}
}

func TestHandleKeysPassesInputThroughWhenPopupActive(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	d.activeInput = true
	for _, r := range []rune{'m', 'i', 't', 'd', 'j', 'k', 'q', 'g'} {
		event := tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone)
		if got := d.handleKeys(event); got != event {
			t.Fatalf("expected rune %q to pass through while input is active", r)
		}
	}
}

func TestHandleKeysUsesLetterShortcutsWhenNoInputActive(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	if got := d.handleKeys(tcell.NewEventKey(tcell.KeyRune, 'd', tcell.ModNone)); got != nil {
		t.Fatalf("expected database shortcut to be handled, got %#v", got)
	}
	if !d.showDatabasePanel {
		t.Fatal("expected d to toggle the database panel")
	}
	if got := d.handleKeys(tcell.NewEventKey(tcell.KeyRune, 'l', tcell.ModNone)); got != nil {
		t.Fatalf("expected log shortcut to be handled, got %#v", got)
	}
	if !d.showSupervisorLog || d.showDatabasePanel {
		t.Fatalf("expected l to toggle supervisor log and hide database panel, got supervisor=%v database=%v", d.showSupervisorLog, d.showDatabasePanel)
	}
}

func TestCompactTUIStatusTruncatesAndRemovesDynamicColorMarkers(t *testing.T) {
	got := compactTUIStatus("one   [two]   three four five", 18)
	if got != "one (two) three..." {
		t.Fatalf("unexpected compact status: %q", got)
	}
}

type recordingTUIDatabaseManager struct {
	prepareCalled bool
	prepareOpts   database.PrismaMigrationAuthoringOptions
}

func (m *recordingTUIDatabaseManager) PreparePrismaMigrationAuthoringDatabase(_ context.Context, opts database.PrismaMigrationAuthoringOptions) (*database.PrismaMigrationAuthoringResult, error) {
	m.prepareCalled = true
	m.prepareOpts = opts
	if opts.Prepare.OnLine != nil {
		opts.Prepare.OnLine("stdout", "database prepared")
	}
	plan := database.PrismaRestorePlan{ExactMatch: true, SnapshotKey: "prisma_001", PrefixLength: 1}
	return &database.PrismaMigrationAuthoringResult{
		Plan: plan,
		Base: &database.PrismaBaseResult{
			Restored: &database.PrismaRestoreResult{Plan: plan},
		},
	}, nil
}

func tuiProgressContains(progress []string, want string) bool {
	for _, message := range progress {
		if strings.Contains(message, want) {
			return true
		}
	}
	return false
}

func TestScrollLogsClampsAtTop(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	d.logs.SetText(strings.Repeat("line\n", 50))
	d.logs.ScrollTo(10, 0)
	d.scrollLogs(-100)
	row, _ := d.logs.GetScrollOffset()
	if row != 0 {
		t.Fatalf("expected log scroll to clamp at top, got row %d", row)
	}
}

func TestLoadSnapshotAllowsMissingInitialStatus(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	inst.LastRun.Target = "up"
	inst.LastRun.Mode = api.ModeDev
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}

	snap, err := loadSnapshot(worktree, inst.ID, false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if snap.state == nil {
		t.Fatal("expected placeholder state")
	}
	if snap.state.Target != "up" {
		t.Fatalf("unexpected placeholder target %q", snap.state.Target)
	}
	if snap.state.Mode != api.ModeDev {
		t.Fatalf("unexpected placeholder mode %q", snap.state.Mode)
	}
	if len(snap.nodes) != 0 {
		t.Fatalf("expected no nodes before initial status, got %d", len(snap.nodes))
	}
}

func TestLoadSnapshotDatabasePanelReadsPrismaSnapshots(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	inst.DB = api.DBInstance{
		Name:          "app_wt_abc",
		Host:          "127.0.0.1",
		Port:          55432,
		ContainerName: "devflow-pg-abc",
		SnapshotRoot:  snapshotRoot,
	}
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SavePrismaSnapshot(snapshotRoot, "prisma_001", &database.PrismaState{
		SchemaHash: "schema",
		Migrations: []database.PrismaMigration{
			{Name: "001_init", Hash: "a"},
		},
		FullHash: "full",
	}); err != nil {
		t.Fatal(err)
	}

	snap, err := loadSnapshot(worktree, inst.ID, false, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if snap.logTitle != databasePanelTitle {
		t.Fatalf("expected database panel snapshot, got %q", snap.logTitle)
	}
	if len(snap.prisma) != 1 || snap.prisma[0].Key != "prisma_001" {
		t.Fatalf("expected loaded Prisma snapshot summary, got %+v", snap.prisma)
	}
}

func mustWriteTUITestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustWriteTUITestExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

type testProject struct {
	name    string
	tasks   []project.Task
	targets []project.Target
}

func (p testProject) Name() string              { return p.name }
func (p testProject) Tasks() []project.Task     { return p.tasks }
func (p testProject) Targets() []project.Target { return p.targets }
func (p testProject) ConfigureInstance(ctx context.Context, worktree string) (project.InstanceConfig, error) {
	return project.InstanceConfig{}, nil
}

type detectorTestProject struct {
	testProject
	marker string
}

func (p detectorTestProject) DetectWorktree(worktree string) bool {
	_, err := os.Stat(filepath.Join(worktree, p.marker))
	return err == nil
}
