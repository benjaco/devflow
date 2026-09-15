package tui

import (
	"context"
	"errors"
	"strings"
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

func TestMigrationActionSelectionUsesOnlyDeclaredTaskRelationships(t *testing.T) {
	p := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("migration-routing")
		first := b.Task("write-first")
		second := b.Task("write-second")
		b.Action("one").Kind(database.ActionMigrationCreate).Task(first).Invalidates("apply-first", "shared")
		b.Action("two").Kind(database.ActionMigrationCreate).Task(second).Invalidates("apply-second", "shared")
		b.Action("unrelated").Kind("another-kind").Task(first).Invalidates("apply-first")
		b.Target("up", first, second)
		return nil
	})
	for _, tc := range []struct{ task, want string }{
		{"write-first", "one"}, {"apply-first", "one"},
		{"write-second", "two"}, {"apply-second", "two"},
		{"shared", ""}, {"apply-first-extra", ""}, {"unrelated", ""}, {"", ""},
	} {
		t.Run(tc.task, func(t *testing.T) {
			action, err := migrationActionForTask(p, tc.task)
			if tc.want == "" {
				if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "one, two") {
					t.Fatalf("unresolved selection %q should explain available actions: %+v, %v", tc.task, action, err)
				}
			} else if err != nil || action.ID != tc.want {
				t.Fatalf("selected %q: got %+v, %v; want %q", tc.task, action, err, tc.want)
			}
		})
	}

	single := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("single-migration")
		author := b.Task("author")
		b.Action("only").Kind(database.ActionMigrationCreate).Task(author)
		b.Target("up", author)
		return nil
	})
	if action, err := migrationActionForTask(single, "unrelated"); err != nil || action.ID != "only" {
		t.Fatalf("single migration action should work from any selection: %+v, %v", action, err)
	}
	if _, err := migrationActionForTask(nil, "anything"); err == nil || !strings.Contains(err.Error(), "no migration-create action") {
		t.Fatalf("missing migration action was not reported: %v", err)
	}
}

func TestMigrationShortcutUsesSelectedTaskWithPrismaAndPayload(t *testing.T) {
	p := project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("tui-selected-migration")
		db := database.Postgres("shared")
		prisma := database.Prisma("accounts").Database(db)
		payload := database.PayloadCMS("catalog").Database(db)
		b.Target("up", b.Group("all", prisma.Migrations(b), payload.Migrations(b)))
		prisma.NewMigration(b)
		payload.NewMigration(b)
		return nil
	})
	project.Register(p)

	for _, tc := range []struct {
		task, other, actionID, label string
		key                          tcell.Key
		char                         rune
		fail                         bool
	}{
		{"accounts_migrations", "catalog_migrations", "accounts.migration.create", "Create Prisma migration", tcell.KeyRune, 'm', false},
		{"catalog_migrations", "accounts_migrations", "catalog.migration.create", "Create PayloadCMS migration", tcell.KeyF4, 0, false},
		{"accounts_migrations", "catalog_migrations", "accounts.migration.create", "Create Prisma migration", tcell.KeyRune, 'm', true},
	} {
		name := tc.task
		if tc.fail {
			name += "/failed_action"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			inst, err := instance.Resolve(root, "migration-test")
			if err != nil {
				t.Fatal(err)
			}
			inst.LastRun.Project = p.Name()
			if err := instance.Save(inst); err != nil {
				t.Fatal(err)
			}
			requests := make(chan daemon.Request, 1)
			previousCall := callDaemonForTUI
			callDaemonForTUI = func(_ context.Context, _ string, req daemon.Request, _ func(api.Event)) (daemon.Response, error) {
				requests <- req
				// Exercise the daemon's real action selector, without running tools or databases.
				_, err := project.ResolveAction(p, req.ActionID, req.ActionKind, req.Component)
				if err == nil && tc.fail {
					err = errors.New("migration command failed")
				}
				return daemon.Response{OK: err == nil}, err
			}
			t.Cleanup(func() { callDaemonForTUI = previousCall })
			d := newDashboard(root, inst.ID)
			d.currentNodes = []api.NodeStatus{{Name: tc.task, Kind: "once", State: api.StateMigrationNeeded}}
			d.selectedName = tc.task
			d.renderTasks(d.currentNodes)
			screen := newObservedSimulationScreen(110, 30)
			d.app.SetScreen(screen)
			done := make(chan error, 1)
			go func() { done <- runTUIApplication(d.app) }()
			t.Cleanup(func() { d.app.Stop(); <-done })
			screen.waitForFrame(t)
			screen.postKey(t, tc.key, tc.char)
			if !dashboardState(t, d, func() bool { return d.activeInput }) {
				t.Fatalf("migration prompt did not open: %s", d.statusMessage)
			}
			if text := simulationScreenText(screen, 110, 30); !strings.Contains(text, tc.label) || !strings.Contains(text, tc.actionID) {
				t.Fatalf("prompt does not identify the selected migration action:\n%s", text)
			}
			screen.postKey(t, tcell.KeyEsc, 0)
			if dashboardState(t, d, func() bool { return d.activeInput || d.busy }) || len(requests) != 0 {
				t.Fatal("canceling a migration prompt dispatched work or left the prompt active")
			}
			screen.postKey(t, tc.key, tc.char)
			screen.postKey(t, tcell.KeyEnter, 0)
			if !dashboardState(t, d, func() bool { return d.activeInput && !d.busy }) || len(requests) != 0 {
				t.Fatal("an empty migration name dispatched work or dismissed the prompt")
			}
			d.app.QueueUpdateDraw(func() {
				d.app.GetFocus().(*tview.InputField).SetText("add-field")
				// Refresh may move selection while a prompt is open; submission must stay bound.
				d.selectedName = tc.other
			})
			screen.postKey(t, tcell.KeyEnter, 0)
			var req daemon.Request
			select {
			case req = <-requests:
			case <-time.After(2 * time.Second):
				t.Fatal("migration prompt did not submit an action")
			}
			deadline := time.Now().Add(3 * time.Second)
			for dashboardState(t, d, func() bool { return d.busy }) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			status := dashboardState(t, d, func() string { return d.statusMessage })
			wantStatus := "created migration"
			if tc.fail {
				wantStatus = "migration failed"
			}
			if req.ActionID != tc.actionID || req.Inputs["name"] != "add-field" || !strings.Contains(status, wantStatus) {
				t.Fatalf("selected task %s submitted action %q, want %q; status=%s", tc.task, req.ActionID, tc.actionID, status)
			}
			if dashboardState(t, d, func() bool { return d.showDatabasePanel }) == tc.fail {
				t.Fatal("failed migration must keep task logs visible; success should show database state")
			}
		})
	}
}
