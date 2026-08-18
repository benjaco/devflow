package tui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/daemon"
	"github.com/benjaco/devflow/pkg/database"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

type observedSimulationScreen struct {
	tcell.SimulationScreen
	frames    chan struct{}
	finalized atomic.Bool
	width     int
	height    int
}

func newObservedSimulationScreen(width, height int) *observedSimulationScreen {
	screen := &observedSimulationScreen{
		SimulationScreen: tcell.NewSimulationScreen("UTF-8"),
		frames:           make(chan struct{}, 32),
		width:            width,
		height:           height,
	}
	return screen
}

func (s *observedSimulationScreen) Init() error {
	if err := s.SimulationScreen.Init(); err != nil {
		return err
	}
	s.SetSize(s.width, s.height)
	return nil
}

func (s *observedSimulationScreen) Show() {
	s.SimulationScreen.Show()
	select {
	case s.frames <- struct{}{}:
	default:
	}
}

func (s *observedSimulationScreen) Fini() {
	s.finalized.Store(true)
	s.SimulationScreen.Fini()
}

func (s *observedSimulationScreen) waitForFrame(t *testing.T) {
	t.Helper()
	select {
	case <-s.frames:
	case <-time.After(time.Second):
		t.Fatal("TUI did not render a frame before the bounded timeout")
	}
}

func (s *observedSimulationScreen) postKey(t *testing.T, key tcell.Key, r rune) {
	t.Helper()
	for len(s.frames) > 0 {
		<-s.frames
	}
	if err := s.PostEvent(tcell.NewEventKey(key, r, tcell.ModNone)); err != nil {
		t.Fatal(err)
	}
	s.waitForFrame(t)
}

func dashboardState[T any](t *testing.T, d *dashboard, read func() T) T {
	t.Helper()
	result := make(chan T, 1)
	d.app.QueueUpdate(func() { result <- read() })
	select {
	case value := <-result:
		return value
	case <-time.After(time.Second):
		t.Fatal("TUI event loop did not process the state read")
		var zero T
		return zero
	}
}

func prepareRunningDashboard(t *testing.T) *dashboard {
	t.Helper()
	d := newDashboard(t.TempDir(), "lock-faithful")
	d.currentNodes = []api.NodeStatus{
		{Name: "alpha", Kind: "once", State: api.StateDone},
		{Name: "beta", Kind: "service", State: api.StateRunning, Ready: true},
	}
	d.allNodes = append([]api.NodeStatus(nil), d.currentNodes...)
	d.selectedName = "alpha"
	d.header.SetText("instance=lock-faithful target=dev mode=watch")
	d.renderTasks(d.currentNodes)
	d.reconcileSelection()
	d.updateLogsFromSnapshot(snapshot{logTitle: "alpha task log", nodes: d.currentNodes, logLines: []string{"ready"}})
	return d
}

func TestApplicationRunDrawsHandlesInputAndExitsWithoutDeadlock(t *testing.T) {
	for _, dimensions := range []struct {
		width  int
		height int
	}{{120, 36}, {100, 30}, {100, 12}, {80, 24}} {
		t.Run(fmt.Sprintf("%dx%d", dimensions.width, dimensions.height), func(t *testing.T) {
			d := prepareRunningDashboard(t)
			screen := newObservedSimulationScreen(dimensions.width, dimensions.height)
			d.app.SetScreen(screen)
			done := make(chan error, 1)
			go func() { done <- runTUIApplication(d.app) }()

			screen.waitForFrame(t)
			if dimensions.width == 120 && dimensions.height == 36 {
				if pane := dashboardState(t, d, func() dashboardPane { return d.focusedPane }); pane != dashboardPaneLogs {
					t.Fatalf("initial focused pane = %v, want logs", pane)
				}
				if title := dashboardState(t, d, func() string { return d.logs.GetTitle() }); !strings.Contains(title, "[FOCUSED]") {
					t.Fatalf("initial log title does not show focus: %q", title)
				}
				screen.postKey(t, tcell.KeyTab, 0)
				if pane := dashboardState(t, d, func() dashboardPane { return d.focusedPane }); pane != dashboardPaneTasks {
					t.Fatalf("focused pane after Tab = %v, want tasks", pane)
				}
				if title := dashboardState(t, d, func() string { return d.tasks.GetTitle() }); !strings.Contains(title, "[FOCUSED]") {
					t.Fatalf("task title does not show focus after Tab: %q", title)
				}
				screen.postKey(t, tcell.KeyDown, 0)
				if selected := dashboardState(t, d, func() string { return d.selectedName }); selected != "beta" {
					t.Fatalf("selected task after Down = %q, want beta", selected)
				}
				screen.postKey(t, tcell.KeyRune, 'k')
				if selected := dashboardState(t, d, func() string { return d.selectedName }); selected != "alpha" {
					t.Fatalf("selected task after k = %q, want alpha", selected)
				}
				screen.postKey(t, tcell.KeyRune, 'j')
				screen.postKey(t, tcell.KeyUp, 0)
				screen.postKey(t, tcell.KeyTab, 0)
				if pane := dashboardState(t, d, func() dashboardPane { return d.focusedPane }); pane != dashboardPaneLogs {
					t.Fatalf("focused pane after second Tab = %v, want logs", pane)
				}

				screen.postKey(t, tcell.KeyRune, '?')
				if open := dashboardState(t, d, func() bool { return d.helpOpen }); !open {
					t.Fatal("contextual help did not open")
				}
				if text := simulationScreenText(screen, dimensions.width, dimensions.height); !strings.Contains(text, "Contextual Help") {
					t.Fatalf("contextual help state was not rendered:\n%s", text)
				}
				screen.postKey(t, tcell.KeyEsc, 0)
				d.app.QueueUpdateDraw(func() {
					d.openLifecyclePlan(api.LifecyclePlan{RequestedAction: "restart", SelectedTask: "beta"}, func() {})
				})
				screen.waitForFrame(t)
				if open := dashboardState(t, d, func() bool { return d.lifecycleOverlay != lifecycleOverlayNone }); !open {
					t.Fatal("lifecycle modal did not render")
				}
				if text := simulationScreenText(screen, dimensions.width, dimensions.height); !strings.Contains(text, "Action: restart") {
					t.Fatalf("lifecycle plan was not rendered:\n%s", text)
				}
				screen.postKey(t, tcell.KeyEsc, 0)
			}

			if err := screen.PostEvent(tcell.NewEventKey(tcell.KeyRune, 'q', tcell.ModNone)); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Application.Run remained blocked after q")
			}
			if !screen.finalized.Load() {
				t.Fatal("simulation screen was not finalized after TUI exit")
			}
		})
	}
}

func TestApplicationLifecyclePreviewConsumesEscapeAndRestoresDashboard(t *testing.T) {
	for _, action := range []string{"rerun", "retarget"} {
		t.Run(action, func(t *testing.T) {
			d := prepareRunningDashboard(t)
			screen := newObservedSimulationScreen(100, 30)
			d.app.SetScreen(screen)
			t.Cleanup(d.app.Stop)
			done := make(chan error, 1)
			go func() { done <- runTUIApplication(d.app) }()

			screen.waitForFrame(t)
			d.app.QueueUpdateDraw(func() {
				d.currentNodes = make([]api.NodeStatus, 24)
				for index := range d.currentNodes {
					d.currentNodes[index] = api.NodeStatus{Name: fmt.Sprintf("task-%02d", index), Kind: "once", State: api.StateDone}
				}
				d.allNodes = append([]api.NodeStatus(nil), d.currentNodes...)
				d.renderTasks(d.currentNodes)
				d.selectedName = "task-14"
				d.tasks.Select(15, 0)
				d.tasks.SetOffset(9, 0)
				d.app.SetFocus(d.logs)
			})
			screen.waitForFrame(t)
			beforeViewport := dashboardState(t, d, func() int {
				row, _ := d.tasks.GetOffset()
				return row
			})
			d.app.QueueUpdateDraw(func() {
				d.openLifecyclePlan(api.LifecyclePlan{RequestedAction: action, SelectedTask: "task-14"}, func() {
					t.Error("canceled lifecycle preview executed its action")
				})
			})
			screen.waitForFrame(t)

			screen.postKey(t, tcell.KeyEsc, 0)
			select {
			case err := <-done:
				t.Fatalf("Escape exited the TUI instead of closing the %s preview: %v", action, err)
			default:
			}
			state := dashboardState(t, d, func() struct {
				modal       bool
				pane        dashboardPane
				selected    string
				viewportRow int
				status      string
			} {
				row, _ := d.tasks.GetOffset()
				return struct {
					modal       bool
					pane        dashboardPane
					selected    string
					viewportRow int
					status      string
				}{d.lifecycleOverlay != lifecycleOverlayNone, d.focusedPane, d.selectedName, row, d.statusMessage}
			})
			if beforeViewport == 0 {
				t.Fatal("test did not establish a scrolled task viewport")
			}
			if state.modal || state.pane != dashboardPaneLogs || state.selected != "task-14" || state.viewportRow != beforeViewport || !strings.Contains(state.status, "canceled") {
				t.Fatalf("dashboard state changed after cancel: %+v", state)
			}

			// The next dashboard event proves Escape was consumed and normal event
			// routing resumes instead of leaving a hidden modal or stopped app.
			screen.postKey(t, tcell.KeyTab, 0)
			screen.postKey(t, tcell.KeyDown, 0)
			if selected := dashboardState(t, d, func() string { return d.selectedName }); selected != "task-15" {
				t.Fatalf("dashboard navigation did not resume after cancel: selected=%q", selected)
			}

			if err := screen.PostEvent(tcell.NewEventKey(tcell.KeyRune, 'q', tcell.ModNone)); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("TUI did not exit after the subsequent dashboard q")
			}
		})
	}
}

func TestApplicationLifecyclePreviewEnterExecutesExactlyOnce(t *testing.T) {
	d := prepareRunningDashboard(t)
	screen := newObservedSimulationScreen(100, 30)
	d.app.SetScreen(screen)
	t.Cleanup(d.app.Stop)
	done := make(chan error, 1)
	go func() { done <- runTUIApplication(d.app) }()
	screen.waitForFrame(t)

	var executions atomic.Int32
	d.app.QueueUpdateDraw(func() {
		d.openLifecyclePlan(api.LifecyclePlan{RequestedAction: "rerun", SelectedTask: "alpha"}, func() {
			executions.Add(1)
		})
	})
	screen.waitForFrame(t)
	screen.postKey(t, tcell.KeyEnter, 0)
	if got := executions.Load(); got != 1 {
		t.Fatalf("lifecycle Enter executions = %d, want 1", got)
	}
	screen.postKey(t, tcell.KeyDown, 0)
	if got := executions.Load(); got != 1 {
		t.Fatalf("dashboard event repeated lifecycle execution: %d", got)
	}

	if err := screen.PostEvent(tcell.NewEventKey(tcell.KeyRune, 'q', tcell.ModNone)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("TUI did not exit after q")
	}
}

func TestApplicationRunRestoresScreenAfterDrawPanic(t *testing.T) {
	d := prepareRunningDashboard(t)
	screen := newObservedSimulationScreen(80, 24)
	d.app.SetScreen(screen)
	originalBeforeDraw := d.app.GetBeforeDrawFunc()
	d.app.SetBeforeDrawFunc(func(screen tcell.Screen) bool {
		originalBeforeDraw(screen)
		panic("synthetic draw panic")
	})
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_ = runTUIApplication(d.app)
	}()
	select {
	case recovered := <-panicked:
		if recovered != "synthetic draw panic" {
			t.Fatalf("recovered panic = %v", recovered)
		}
	case <-time.After(time.Second):
		t.Fatal("draw panic left Application.Run blocked")
	}
	if !screen.finalized.Load() {
		t.Fatal("screen was not finalized after draw panic")
	}
}

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

func TestOrderNodesForTUIUsesStableGraphOrderAcrossStateChanges(t *testing.T) {
	const name = "tui-stable-order-project"
	p := testProject{
		name: name,
		tasks: []project.Task{
			{Name: "build", Kind: project.KindOnce},
			{Name: "backend", Kind: project.KindService, Deps: []string{"build"}},
			{Name: "frontend", Kind: project.KindService},
		},
		targets: []project.Target{{Name: "dev", RootTasks: []string{"backend", "frontend"}}},
	}
	project.Register(p)
	inst := &api.Instance{LastRun: api.RunConfig{Project: name, Target: "dev"}}
	nodes := []api.NodeStatus{
		{Name: "frontend", State: api.StateRunning},
		{Name: "backend", State: api.StatePending},
		{Name: "build", State: api.StateDone},
	}
	orderNodesForTUI(nodes, inst, "dev")
	first := []string{nodes[0].Name, nodes[1].Name, nodes[2].Name}
	for index := range nodes {
		nodes[index].State = []api.NodeState{api.StateFailed, api.StateRunning, api.StateCached}[index]
	}
	orderNodesForTUI(nodes, inst, "dev")
	second := []string{nodes[0].Name, nodes[1].Name, nodes[2].Name}
	if strings.Join(first, ",") != "build,backend,frontend" || strings.Join(second, ",") != strings.Join(first, ",") {
		t.Fatalf("state transition reordered task rows: before=%v after=%v", first, second)
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
				Image:         "postgres:16.14",
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
		"flavor=postgres",
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

func TestRenderDatabasePanelShowsArchitectureSelectedPostGISFlavor(t *testing.T) {
	snap := snapshot{
		instance: &api.Instance{DB: api.DBInstance{
			Name:            "geo_wt_abc",
			Host:            "127.0.0.1",
			Port:            55432,
			User:            "devflow",
			Flavor:          database.FlavorPostGIS,
			PostgresVersion: 18,
			ContainerName:   "devflow-pg-abc",
		}},
		logTitle: databasePanelTitle,
	}
	joined := strings.Join(renderLogPanel(snap, ""), "\n")
	if !strings.Contains(joined, "flavor=postgis postgres=18 image=auto") {
		t.Fatalf("expected architecture-selected PostGIS flavor in database panel, got:\n%s", joined)
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

func TestGeneratePrismaMigrationFromTUIRunsProjectTargetAndRelaunches(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	var gotRoot string
	var gotReq daemon.Request
	previousCall := callDaemonForTUI
	callDaemonForTUI = func(ctx context.Context, root string, req daemon.Request, onEvent func(api.Event)) (daemon.Response, error) {
		_ = ctx
		gotRoot = root
		gotReq = req
		if onEvent != nil {
			onEvent(api.Event{Type: api.EventRunStarted, Target: "prisma_new_migration"})
			onEvent(api.Event{Type: api.EventTaskState, Task: "prisma_new_migration", State: api.StateRunning})
			onEvent(api.Event{Type: api.EventLogLine, Task: "prisma_new_migration", Stream: "stdout", Line: "created migration add-age"})
			onEvent(api.Event{Type: api.EventLogLine, Task: "daemon", Stream: "status", Line: "detached target up relaunched"})
			success := true
			onEvent(api.Event{Type: api.EventRunFinished, Target: "prisma_new_migration", Success: &success})
		}
		return daemon.Response{OK: true}, nil
	}
	t.Cleanup(func() { callDaemonForTUI = previousCall })

	var progress []string
	if err := generatePrismaMigrationFromTUI(worktree, inst.ID, "add-age", func(message string) {
		progress = append(progress, message)
	}); err != nil {
		t.Fatal(err)
	}
	if gotRoot != worktree {
		t.Fatalf("unexpected daemon root: got %q want %q", gotRoot, worktree)
	}
	if gotReq.Action != daemon.ActionRunAction || gotReq.ActionKind != database.ActionMigrationCreate || !gotReq.StreamEvents || gotReq.Inputs["name"] != "add-age" {
		t.Fatalf("unexpected daemon request: %+v", gotReq)
	}
	for _, want := range []string{
		"connecting to daemon",
		"run started: prisma_new_migration",
		"prisma_new_migration: running",
		"prisma_new_migration stdout: created migration add-age",
		"detached target up relaunched",
		"run finished: prisma_new_migration",
	} {
		if !tuiProgressContains(progress, want) {
			t.Fatalf("expected progress to contain %q, got %+v", want, progress)
		}
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

func TestFooterAndContextualHelpRespectModalAndInputSemantics(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	d.setStatus("[red]restart failed: broken pipe")
	footer := d.footer.GetText(false)
	if !strings.Contains(footer, "restart failed") || !strings.Contains(footer, "?") {
		t.Fatalf("action status/help was not permanently visible: %q", footer)
	}
	if got := d.handleKeys(tcell.NewEventKey(tcell.KeyRune, '?', tcell.ModNone)); got != nil || !d.helpOpen {
		t.Fatalf("? did not open contextual help: event=%v open=%v", got, d.helpOpen)
	}
	if got := d.handleKeys(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone)); got != nil || d.helpOpen {
		t.Fatalf("Escape did not return from help: event=%v open=%v", got, d.helpOpen)
	}
	d.activeInput = true
	d.renderFooter()
	footer = d.footer.GetText(false)
	if strings.Contains(footer, "q quit") || !strings.Contains(footer, "Enter accept") || !strings.Contains(footer, "Escape cancel") {
		t.Fatalf("text-input footer advertised global shortcuts: %q", footer)
	}
}

func TestLifecyclePlanEscapeCancelsWithoutExecuting(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	executed := false
	d.openLifecyclePlan(api.LifecyclePlan{
		RequestedAction:    "rerun",
		SelectedTask:       "backend_debug",
		ProcessesToStop:    []string{"backend_debug"},
		TasksToInvalidate:  []string{},
		TasksToExecute:     []string{"backend_debug"},
		ServicesToPreserve: []string{"frontend"},
		ServicesToRestart:  []string{"backend_debug"},
	}, func() { executed = true })
	d.handleKeys(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	if executed || d.lifecycleOverlay != lifecycleOverlayNone || !strings.Contains(d.statusMessage, "canceled") {
		t.Fatalf("Escape changed execution state: executed=%v overlay=%v status=%q", executed, d.lifecycleOverlay, d.statusMessage)
	}
}

func TestApplicationRetargetChooserIsVerticalScrollableAndCancelable(t *testing.T) {
	d := prepareRunningDashboard(t)
	screen := newObservedSimulationScreen(80, 24)
	d.app.SetScreen(screen)
	t.Cleanup(d.app.Stop)
	done := make(chan error, 1)
	go func() { done <- runTUIApplication(d.app) }()
	screen.waitForFrame(t)

	targets := []string{
		"unit", "backend", "dev", "ci", "deploy", "docs", "e2e", "frontend", "lint", "migration",
		"production_readiness_with_a_complete_selected_name", "release", "smoke", "staticcheck", "test",
	}
	d.app.QueueUpdateDraw(func() {
		d.app.SetFocus(d.logs)
		d.openRetargetChooser("dev", targets)
	})
	screen.waitForFrame(t)

	state := dashboardState(t, d, func() struct {
		ordered     []string
		selected    string
		currentItem string
		detail      string
	} {
		index := d.retargetList.GetCurrentItem()
		currentIndex := slices.Index(d.retargetTargets, "dev")
		currentItem, _ := d.retargetList.GetItemText(currentIndex)
		return struct {
			ordered     []string
			selected    string
			currentItem string
			detail      string
		}{append([]string(nil), d.retargetTargets...), d.retargetTargets[index], currentItem, d.retargetDetail.GetText(false)}
	})
	wantOrder := append([]string(nil), targets...)
	sort.Strings(wantOrder)
	if !slices.Equal(state.ordered, wantOrder) {
		t.Fatalf("retarget ordering = %v, want %v", state.ordered, wantOrder)
	}
	if state.selected == "dev" || !strings.Contains(state.currentItem, "dev") || !strings.Contains(state.currentItem, "current") || !strings.Contains(state.detail, state.selected) {
		t.Fatalf("retarget initial/current state is unclear: selected=%q current=%q detail=%q", state.selected, state.currentItem, state.detail)
	}
	const longTarget = "production_readiness_with_a_complete_selected_name"
	d.app.QueueUpdateDraw(func() { d.retargetList.SetCurrentItem(slices.Index(wantOrder, longTarget)) })
	screen.waitForFrame(t)
	if detail := dashboardState(t, d, func() string { return d.retargetDetail.GetText(false) }); !strings.Contains(detail, longTarget) {
		t.Fatalf("selected long target was not retained in full: %q", detail)
	}
	if rendered := simulationScreenText(screen, 80, 24); !strings.Contains(rendered, longTarget) {
		t.Fatalf("selected long target was clipped from the rendered chooser:\n%s", rendered)
	}

	// Home plus actual Down/j/k events prove the list scroll path can reach
	// every stable entry without falling through to dashboard navigation.
	screen.postKey(t, tcell.KeyHome, 0)
	visited := map[string]bool{}
	for index := range wantOrder {
		selected := dashboardState(t, d, func() string {
			return d.retargetTargets[d.retargetList.GetCurrentItem()]
		})
		visited[selected] = true
		if index+1 < len(wantOrder) {
			screen.postKey(t, tcell.KeyDown, 0)
		}
	}
	if len(visited) != len(wantOrder) {
		t.Fatalf("reachable retarget choices = %d, want %d: %v", len(visited), len(wantOrder), visited)
	}
	screen.postKey(t, tcell.KeyRune, 'k')
	screen.postKey(t, tcell.KeyRune, 'j')
	selected := dashboardState(t, d, func() string {
		return d.retargetTargets[d.retargetList.GetCurrentItem()]
	})
	detail := dashboardState(t, d, func() string { return d.retargetDetail.GetText(false) })
	if !strings.Contains(detail, selected) {
		t.Fatalf("selected target is clipped from chooser detail: selected=%q detail=%q", selected, detail)
	}

	screen.postKey(t, tcell.KeyEsc, 0)
	select {
	case err := <-done:
		t.Fatalf("retarget Escape exited the TUI: %v", err)
	default:
	}
	if pane := dashboardState(t, d, func() dashboardPane { return d.focusedPane }); pane != dashboardPaneLogs {
		t.Fatalf("retarget cancel restored pane %v, want logs", pane)
	}
	screen.postKey(t, tcell.KeyTab, 0)
	screen.postKey(t, tcell.KeyDown, 0)
	if selected := dashboardState(t, d, func() string { return d.selectedName }); selected != "beta" {
		t.Fatalf("dashboard navigation did not resume after chooser cancel: %q", selected)
	}

	if err := screen.PostEvent(tcell.NewEventKey(tcell.KeyRune, 'q', tcell.ModNone)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("TUI did not exit after q")
	}
}

func TestApplicationRetargetChooserEnterOpensImpactPreviewOnce(t *testing.T) {
	previousCall := callDaemonForTUI
	var previews atomic.Int32
	callDaemonForTUI = func(_ context.Context, _ string, req daemon.Request, _ func(api.Event)) (daemon.Response, error) {
		previews.Add(1)
		if req.Action != daemon.ActionRetarget || req.Target != "next" || !req.Preview {
			t.Errorf("unexpected retarget preview request: %+v", req)
		}
		return daemon.Response{OK: true, Lifecycle: &api.LifecycleResult{Plan: api.LifecyclePlan{
			RequestedAction:   "retarget",
			SelectedTarget:    "next",
			TasksToExecute:    []string{"backend"},
			ServicesToRestart: []string{"backend"},
		}}}, nil
	}
	t.Cleanup(func() { callDaemonForTUI = previousCall })

	d := prepareRunningDashboard(t)
	screen := newObservedSimulationScreen(100, 30)
	d.app.SetScreen(screen)
	t.Cleanup(d.app.Stop)
	done := make(chan error, 1)
	go func() { done <- runTUIApplication(d.app) }()
	screen.waitForFrame(t)
	d.app.QueueUpdateDraw(func() { d.openRetargetChooser("current", []string{"next", "current"}) })
	screen.waitForFrame(t)
	screen.postKey(t, tcell.KeyDown, 0)
	screen.postKey(t, tcell.KeyEnter, 0)

	deadline := time.Now().Add(time.Second)
	for {
		if dashboardState(t, d, func() lifecycleOverlayKind { return d.lifecycleOverlay }) == lifecycleOverlayPlan {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retarget Enter did not open the lifecycle impact preview")
		}
		time.Sleep(time.Millisecond)
	}
	if got := previews.Load(); got != 1 {
		t.Fatalf("retarget preview requests = %d, want 1", got)
	}
	screen.postKey(t, tcell.KeyEsc, 0)
	select {
	case err := <-done:
		t.Fatalf("impact-preview Escape exited the TUI: %v", err)
	default:
	}

	if err := screen.PostEvent(tcell.NewEventKey(tcell.KeyRune, 'q', tcell.ModNone)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("TUI did not exit after q")
	}
}

func TestLiveLogFollowingPauseResumeAndTaskSwitch(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	d.selectedName = "backend"
	backendPath := filepath.Join(t.TempDir(), "backend.log")
	snap := snapshot{
		logTitle: "backend task log",
		logPath:  backendPath,
		logLines: []string{"one", "two", "three"},
		nodes:    []api.NodeStatus{{Name: "backend", State: api.StateRunning}},
	}
	d.updateLogsFromSnapshot(snap)
	if !d.logFollowing || !strings.Contains(d.logs.GetTitle(), "FOLLOWING") {
		t.Fatalf("running log did not open in follow mode: %q", d.logs.GetTitle())
	}
	d.scrollLogs(-1)
	snap.logLines = append(snap.logLines, "four")
	d.updateLogsFromSnapshot(snap)
	if d.logFollowing || !strings.Contains(d.logs.GetTitle(), "PAUSED") {
		t.Fatalf("manual upward scroll did not pause following: %q", d.logs.GetTitle())
	}
	d.resumeLogFollowing()
	if !d.logFollowing || !strings.Contains(d.logs.GetTitle(), "FOLLOWING") {
		t.Fatalf("f/End resume did not restore following: %q", d.logs.GetTitle())
	}
	d.selectedName = "frontend"
	d.updateLogsFromSnapshot(snapshot{
		logTitle: "frontend task log",
		logPath:  filepath.Join(t.TempDir(), "frontend.log"),
		logLines: []string{"fresh"},
		nodes:    []api.NodeStatus{{Name: "frontend", State: api.StateRunning}},
	})
	if !d.logFollowing {
		t.Fatal("switching task logs did not reset to follow mode")
	}
}

func TestLoadOlderLogContentPreservesPausedLogicalPosition(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "older-log")
	if err != nil {
		t.Fatal(err)
	}
	logPath := instance.LogPath(worktree, inst.ID, "backend")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	for line := 1; line <= 500; line++ {
		_, _ = fmt.Fprintf(&content, "stdout: logical line %03d\n", line)
	}
	if err := os.WriteFile(logPath, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := instance.SaveStatus(worktree, inst.ID, "dev", api.ModeWatch, map[string]api.NodeStatus{
		"backend": {Name: "backend", Kind: string(project.KindService), State: api.StateRunning, LogPath: logPath},
	}); err != nil {
		t.Fatal(err)
	}

	d := newDashboard(worktree, inst.ID)
	d.selectedName = "backend"
	d.updateLogs()
	if d.lastSnapshot.logStartLine != 301 || d.lastSnapshot.logEndLine != 500 || !d.lastSnapshot.logTruncated {
		t.Fatalf("unexpected initial retained range: %d-%d truncated=%v", d.lastSnapshot.logStartLine, d.lastSnapshot.logEndLine, d.lastSnapshot.logTruncated)
	}
	d.logFollowing = false
	d.logs.ScrollTo(25, 0)
	d.rememberLogScroll(d.renderedLogKey, 25, 0)
	d.loadOlderLogContent()

	row, col := d.desiredLogScroll(d.renderedLogKey)
	if d.logFollowing || row != 225 || col != 0 {
		t.Fatalf("older history changed paused logical position: following=%v row=%d col=%d", d.logFollowing, row, col)
	}
	if d.lastSnapshot.logStartLine != 101 || d.lastSnapshot.logEndLine != 500 {
		t.Fatalf("unexpected expanded retained range: %d-%d", d.lastSnapshot.logStartLine, d.lastSnapshot.logEndLine)
	}

	// Appending live output while paused must leave the same logical content
	// under the viewport rather than snapping back to the tail.
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("stdout: logical line 501\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	d.updateLogs()
	row, col = d.desiredLogScroll(d.renderedLogKey)
	if d.logFollowing || row != 225 || col != 0 {
		t.Fatalf("background refresh changed paused viewport: following=%v row=%d col=%d", d.logFollowing, row, col)
	}
}

func TestLoadOlderLogContentWithoutHistoryPreservesFollowPreference(t *testing.T) {
	d := newDashboard(t.TempDir(), "complete-log")
	d.selectedName = "backend"
	d.updateLogsFromSnapshot(snapshot{
		logTitle:     "backend log",
		logPath:      filepath.Join(t.TempDir(), "backend.log"),
		logLines:     []string{"one", "two", "three"},
		logStartLine: 1,
		logEndLine:   3,
		nodes:        []api.NodeStatus{{Name: "backend", State: api.StateRunning}},
	})
	d.logFollowing = false
	d.logs.ScrollTo(1, 0)
	d.rememberLogScroll(d.renderedLogKey, 1, 0)
	d.loadOlderLogContent()
	row, col := d.desiredLogScroll(d.renderedLogKey)
	if d.logFollowing || row != 1 || col != 0 || d.logLineLimit != 200 {
		t.Fatalf("no-history load changed paused state: following=%v row=%d col=%d limit=%d", d.logFollowing, row, col, d.logLineLimit)
	}
}

func TestExplicitLogFollowKeysResumeAtTail(t *testing.T) {
	for name, event := range map[string]*tcell.EventKey{
		"f":   tcell.NewEventKey(tcell.KeyRune, 'f', tcell.ModNone),
		"End": tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone),
		"G":   tcell.NewEventKey(tcell.KeyRune, 'G', tcell.ModNone),
	} {
		t.Run(name, func(t *testing.T) {
			d := newDashboard(t.TempDir(), "follow-key")
			d.selectedName = "backend"
			d.updateLogsFromSnapshot(snapshot{
				logTitle: "backend log",
				logPath:  filepath.Join(t.TempDir(), "backend.log"),
				logLines: []string{"one", "two", "three"},
				nodes:    []api.NodeStatus{{Name: "backend", State: api.StateRunning}},
			})
			d.focusedPane = dashboardPaneLogs
			d.logFollowing = false
			d.logs.ScrollToBeginning()
			d.rememberLogScroll(d.renderedLogKey, 0, 0)
			if got := d.handleKeys(event); got != nil {
				t.Fatalf("follow key was not consumed: %#v", got)
			}
			if !d.logFollowing || !strings.Contains(d.logs.GetTitle(), "FOLLOWING") {
				t.Fatalf("%s did not resume live following: following=%v title=%q", name, d.logFollowing, d.logs.GetTitle())
			}
		})
	}
}

func TestReadLastLinesIsBoundedAndReportsRetainedRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.log")
	var content strings.Builder
	for i := 1; i <= 300; i++ {
		_, _ = fmt.Fprintf(&content, "line %03d\n", i)
	}
	content.WriteString(strings.Repeat("x", 128*1024))
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := readLastLinesInfo(path, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.lines) != 200 || info.startLine != 102 || info.endLine != 301 || !info.truncated {
		t.Fatalf("unexpected bounded tail metadata: %+v", info)
	}
	if last := info.lines[len(info.lines)-1]; len(last) > 17*1024 || !strings.Contains(last, "line truncated") {
		t.Fatalf("oversized line was not safely truncated: bytes=%d tail=%q", len(last), last[max(0, len(last)-32):])
	}
}

func TestLifecycleBadgesAreDistinctWithoutColor(t *testing.T) {
	states := []api.NodeState{
		api.StatePending, api.StateStarting, api.StateRunning, api.StateReady,
		api.StateRestarting, api.StateCached, api.StateDone, api.StateFailed,
		api.StateCanceled, api.StateBlocked, api.StateStopped, api.StateDegraded,
		api.StateDirty,
	}
	seen := map[string]api.NodeState{}
	for _, state := range states {
		badge := stateBadge(state)
		if prior, exists := seen[badge]; exists {
			t.Fatalf("states %q and %q share monochrome badge %q", prior, state, badge)
		}
		seen[badge] = state
	}
	if reason := nodeStateReason(api.NodeStatus{State: api.StateBlocked, LastError: "waiting for database"}); !strings.Contains(reason, "database") {
		t.Fatalf("blocking reason was hidden: %q", reason)
	}
}

func TestResponsiveDashboardLayouts(t *testing.T) {
	cases := []struct {
		width  int
		height int
		want   []string
	}{
		{60, 24, []string{"? help", "backend_debug"}},
		{80, 24, []string{"backend_debug", "restart failed"}},
		{100, 12, []string{"backend_debug", "RUN"}},
		{100, 30, []string{"backend_debug", "task log"}},
		{140, 40, []string{"backend_debug", "task log", "restart failed"}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%dx%d", tc.width, tc.height), func(t *testing.T) {
			d := newDashboard(t.TempDir(), "abc123")
			d.currentNodes = []api.NodeStatus{{Name: "backend_debug", Kind: "debug_service", State: api.StateRunning}}
			d.selectedName = "backend_debug"
			d.header.SetText("instance=abc123 target=dev mode=watch\nworktree=/tmp/example\ndb=running\nurls=backend\nstates=RUN=1")
			d.renderTasks(d.currentNodes)
			d.updateLogsFromSnapshot(snapshot{
				logTitle: "backend_debug task log",
				logLines: []string{"service output"},
				nodes:    d.currentNodes,
			})
			d.setStatus("[red]restart failed: broken pipe")
			d.applyResponsiveLayout(tc.width, tc.height)
			screen := tcell.NewSimulationScreen("UTF-8")
			if err := screen.Init(); err != nil {
				t.Fatal(err)
			}
			defer screen.Fini()
			screen.SetSize(tc.width, tc.height)
			d.pages.SetRect(0, 0, tc.width, tc.height)
			d.pages.Draw(screen)
			text := simulationScreenText(screen, tc.width, tc.height)
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Fatalf("layout missing %q:\n%s", want, text)
				}
			}
		})
	}
}

func simulationScreenText(screen tcell.Screen, width, height int) string {
	var text strings.Builder
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			cell, _, _ := screen.Get(x, y)
			if cell == "" {
				cell = " "
			}
			text.WriteString(cell)
		}
		text.WriteByte('\n')
	}
	return text.String()
}

func TestDetachedQuitMessageIncludesInspectAndStopCommands(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	inst.LastRun = api.RunConfig{Target: "dev", Detached: true}
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	writeDetachedQuitMessage(&output, worktree, inst.ID)
	message := output.String()
	for _, want := range []string{"remains active", "devflow status", "devflow stop", inst.ID, "dev"} {
		if !strings.Contains(message, want) {
			t.Fatalf("quit message missing %q: %q", want, message)
		}
	}
}

func TestUpdateLogsKeepsPreviousLinesDuringTransientEmptyRead(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	logPath := filepath.Join(t.TempDir(), "task.log")
	d.updateLogsFromSnapshot(snapshot{
		logTitle: "task log",
		logPath:  logPath,
		logLines: []string{"stdout: first line"},
		nodes:    []api.NodeStatus{{Name: "task", State: api.StateRunning}},
	})
	d.updateLogsFromSnapshot(snapshot{
		logTitle: "task log",
		logPath:  logPath,
		nodes:    []api.NodeStatus{{Name: "task", State: api.StateRunning}},
	})
	text := d.logs.GetText(false)
	if !strings.Contains(text, "stdout: first line") {
		t.Fatalf("expected previous log content to remain visible, got %q", text)
	}
}

func TestUpdateLogsPreservesScrollForSameLogReload(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	logPath := filepath.Join(t.TempDir(), "task.log")
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("stdout: line %02d", i)
	}
	d.selectedName = "task"
	d.updateLogsFromSnapshot(snapshot{
		logTitle: "task log",
		logPath:  logPath,
		logLines: lines,
		nodes:    []api.NodeStatus{{Name: "task", State: api.StateRunning}},
	})
	d.scrollLogs(12)

	lines = append(lines, "stdout: new line")
	d.updateLogsFromSnapshot(snapshot{
		logTitle: "task log",
		logPath:  logPath,
		logLines: lines,
		nodes:    []api.NodeStatus{{Name: "task", State: api.StateRunning}},
	})

	row, col := d.logs.GetScrollOffset()
	if row != 12 || col != 0 {
		t.Fatalf("expected scroll offset to survive same-log reload, got row=%d col=%d", row, col)
	}
}

func TestUpdateLogsRestoresDesiredScrollAfterTemporaryShortReload(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	logPath := filepath.Join(t.TempDir(), "task.log")
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("stdout: line %02d", i)
	}
	d.selectedName = "task"
	snap := snapshot{
		logTitle: "task log",
		logPath:  logPath,
		logLines: lines,
		nodes:    []api.NodeStatus{{Name: "task", State: api.StateRunning}},
	}
	d.updateLogsFromSnapshot(snap)
	d.scrollLogs(12)

	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	d.logs.SetRect(0, 0, 80, 8)

	snap.logLines = []string{"stdout: restarting"}
	d.updateLogsFromSnapshot(snap)
	d.logs.Draw(screen)
	if row, _ := d.logs.GetScrollOffset(); row != 0 {
		t.Fatalf("expected tview to clamp the visible short-log offset, got row %d", row)
	}

	snap.logLines = lines
	d.updateLogsFromSnapshot(snap)
	row, col := d.logs.GetScrollOffset()
	if row != 12 || col != 0 {
		t.Fatalf("expected desired scroll to be restored after full log reload, got row=%d col=%d", row, col)
	}
}

func TestLogMouseCaptureForwardsNativeScrollAndRecordsDesiredOffset(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	logPath := filepath.Join(t.TempDir(), "task.log")
	d.selectedName = "task"
	d.updateLogsFromSnapshot(snapshot{
		logTitle: "task log",
		logPath:  logPath,
		logLines: []string{"stdout: first", "stdout: second"},
		nodes:    []api.NodeStatus{{Name: "task", State: api.StateRunning}},
	})

	capture := d.logs.GetMouseCapture()
	if capture == nil {
		t.Fatal("expected log mouse capture to be installed")
	}
	action, event := capture(tview.MouseScrollDown, tcell.NewEventMouse(0, 0, tcell.WheelDown, 0))
	if action != tview.MouseScrollDown || event == nil {
		t.Fatalf("expected native scroll event to be forwarded, got action=%v event=%v", action, event)
	}
	if d.logScrollRow != 1 {
		t.Fatalf("expected desired scroll row to be recorded, got %d", d.logScrollRow)
	}
}

func TestUpdateLogsResetsScrollWhenLogChanges(t *testing.T) {
	d := newDashboard(t.TempDir(), "abc123")
	firstLog := filepath.Join(t.TempDir(), "first.log")
	secondLog := filepath.Join(t.TempDir(), "second.log")
	d.selectedName = "first"
	d.updateLogsFromSnapshot(snapshot{
		logTitle: "first log",
		logPath:  firstLog,
		logLines: []string{"stdout: first"},
		nodes:    []api.NodeStatus{{Name: "first", State: api.StateRunning}},
	})
	d.logs.ScrollTo(9, 0)

	d.selectedName = "second"
	d.updateLogsFromSnapshot(snapshot{
		logTitle: "second log",
		logPath:  secondLog,
		logLines: []string{"stdout: second"},
		nodes:    []api.NodeStatus{{Name: "second", State: api.StateRunning}},
	})

	row, _ := d.logs.GetScrollOffset()
	if row != 0 {
		t.Fatalf("expected scroll offset to reset when switching logs, got row %d", row)
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

func TestMaybeStopDaemonForTUIOnlyStopsOwnedDaemon(t *testing.T) {
	previousStop := stopDaemonForTUI
	calls := 0
	stopDaemonForTUI = func(ctx context.Context, client *daemon.Client) error {
		_ = client
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("expected shutdown call to have a deadline")
		}
		calls++
		return nil
	}
	t.Cleanup(func() { stopDaemonForTUI = previousStop })

	if err := maybeStopDaemonForTUI(nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("expected unowned daemon to be left running, got %d stop calls", calls)
	}
	if err := maybeStopDaemonForTUI(nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected owned daemon to be stopped once, got %d stop calls", calls)
	}
}

func TestCompactTUIStatusTruncatesAndRemovesDynamicColorMarkers(t *testing.T) {
	got := compactTUIStatus("one   [two]   three four five", 18)
	if got != "one (two) three..." {
		t.Fatalf("unexpected compact status: %q", got)
	}
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

func TestLoadSnapshotOverlaysLastRunOnEmptyPersistedStatus(t *testing.T) {
	worktree := t.TempDir()
	inst, err := instance.Resolve(worktree, "test")
	if err != nil {
		t.Fatal(err)
	}
	inst.LastRun.Target = "up"
	inst.LastRun.Mode = api.ModeWatch
	if err := instance.Save(inst); err != nil {
		t.Fatal(err)
	}
	if err := instance.SaveStatus(worktree, inst.ID, "", "", map[string]api.NodeStatus{}); err != nil {
		t.Fatal(err)
	}

	snap, err := loadSnapshot(worktree, inst.ID, false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if snap.state.Target != "up" || snap.state.Mode != api.ModeWatch {
		t.Fatalf("expected last run target/mode overlay, got target=%q mode=%q", snap.state.Target, snap.state.Mode)
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
