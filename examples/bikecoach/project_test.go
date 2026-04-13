package bikecoach

import (
	"context"
	"path/filepath"
	"testing"

	"devflow/pkg/api"
	"devflow/pkg/graph"
	"devflow/pkg/project"
)

func TestBikecoachProjectRegistered(t *testing.T) {
	p, err := project.Lookup("bikecoach")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Name(); got != "bikecoach" {
		t.Fatalf("unexpected project name %q", got)
	}
}

func TestBikecoachProjectDetectionAndDefaultTarget(t *testing.T) {
	worktree := t.TempDir()
	if err := SeedWorktree(worktree); err != nil {
		t.Fatal(err)
	}
	p, err := project.Detect(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Name(); got != "bikecoach" {
		t.Fatalf("unexpected detected project %q", got)
	}
	if got := project.PreferredTarget(p); got != "up" {
		t.Fatalf("unexpected default target %q", got)
	}
}

func TestBikecoachGraphValidates(t *testing.T) {
	p := bikecoachProject{}
	g, err := graph.New(p.Tasks(), p.Targets())
	if err != nil {
		t.Fatal(err)
	}
	closure, err := g.TargetClosure("fullstack")
	if err != nil {
		t.Fatal(err)
	}
	if len(closure) == 0 {
		t.Fatal("expected fullstack closure to be non-empty")
	}
	required := []string{
		"build_frontend_main",
		"build_frontend_internal",
		"build_frontend_admin",
		"sqlc_generate",
		"build_tools",
		"build_coach",
		"prepare_db_base",
		"db_migrate",
		"postgres",
		"backend_dev",
	}
	for _, name := range required {
		if _, ok := g.Tasks[name]; !ok {
			t.Fatalf("expected task %q to be registered", name)
		}
	}
}

func TestBikecoachConfigureInstanceAppliesOverrides(t *testing.T) {
	worktree := t.TempDir()
	if err := SeedWorktree(worktree); err != nil {
		t.Fatal(err)
	}

	cfg, err := bikecoachProject{}.ConfigureInstance(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	inst := &api.Instance{
		ID:       "bikecoach123456",
		Label:    filepath.Base(worktree),
		Worktree: worktree,
		Ports: map[string]int{
			"backend":  4010,
			"postgres": 55444,
		},
		Env: cfg.Env,
	}
	if err := cfg.Finalize(inst); err != nil {
		t.Fatal(err)
	}
	if got := inst.Env["PORT"]; got != "4010" {
		t.Fatalf("expected PORT override, got %q", got)
	}
	if got := inst.Env["PGPORT"]; got != "55444" {
		t.Fatalf("expected PGPORT override, got %q", got)
	}
	if got := inst.Env["DATABASE_URL"]; got == "" || got == "postgres://coach:coach@localhost:5432/coach?sslmode=disable" {
		t.Fatalf("expected DATABASE_URL to be replaced with instance DB URL, got %q", got)
	}
	if got := inst.Env["STRAVA_REDIRECT_URI"]; got != "http://127.0.0.1:4010/oauth/callback" {
		t.Fatalf("unexpected STRAVA_REDIRECT_URI %q", got)
	}
	if got := inst.Env["DEV_AUTH_BYPASS"]; got != "1" {
		t.Fatalf("expected DEV_AUTH_BYPASS default, got %q", got)
	}
}
