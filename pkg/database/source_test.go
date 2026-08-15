package database

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/benjaco/devflow/internal/testutil"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

func TestCommandSourcePolicyMergesAdapterAndDatabaseEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test is unix-only")
	}
	worktree := t.TempDir()
	output := filepath.Join(worktree, "source.txt")
	policy := CommandSourcePolicy{
		PolicyName: "clone-dev",
		Spec: process.CommandSpec{
			Name: "sh",
			Args: []string{"-c", "printf '%s|%s|%s' \"$REMOTE_URL\" \"$PGDATABASE\" \"$DATABASE_URL\" > \"$OUT_FILE\""},
		},
	}
	db := api.DBInstance{
		Name: "app_wt_abc",
		URL:  "postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
		Host: "127.0.0.1",
		Port: 55432,
		User: "devflow",
	}
	err := policy.PrepareBase(context.Background(), db, PrepareOptions{
		Worktree: worktree,
		Env: map[string]string{
			"REMOTE_URL": "postgres://remote/dev",
			"OUT_FILE":   output,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(data))
	want := "postgres://remote/dev|app_wt_abc|postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable"
	if got != want {
		t.Fatalf("unexpected command source output %q, want %q", got, want)
	}
}

func TestPostgresDumpSourcePolicyClonesRemoteIntoLocalDatabase(t *testing.T) {
	worktree := t.TempDir()
	binDir := installFakePostgresClients(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output := filepath.Join(worktree, "dump.txt")
	audit := filepath.Join(worktree, "postgres-audit.txt")
	t.Setenv("DEVFLOW_FAKE_PG_AUDIT", audit)
	policy := PostgresDumpSourcePolicy{
		PolicyName: "clone-dev",
		RemoteURL:  "postgres://remote-user:remote-secret@remote.example:5433/dev?sslmode=require",
	}
	db := api.DBInstance{
		Name: "app_wt_abc",
		URL:  "postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
		Host: "127.0.0.1",
		Port: 55432,
		User: "devflow",
	}
	if err := policy.PrepareBase(context.Background(), db, PrepareOptions{
		Worktree: worktree,
		Env: map[string]string{
			"OUT_FILE":         output,
			"DEV_DATABASE_URL": "postgres://remote-user:remote-secret@remote.example:5433/dev?sslmode=require",
		},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "dump:postgres://remote.example:5433/dev?sslmode=require" {
		t.Fatalf("unexpected dump output %q", string(data))
	}
	url, err := os.ReadFile(output + ".url")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(url)) != "postgres://127.0.0.1:55432/app_wt_abc?sslmode=disable" {
		t.Fatalf("unexpected local database url %q", string(url))
	}
	auditData, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	auditText := string(auditData)
	if strings.Contains(auditText, "remote-secret") || strings.Contains(auditText, "secret@") {
		t.Fatalf("database credentials leaked into process arguments/environment audit: %s", auditText)
	}
	if got := strings.Count(auditText, "pgpass_mode=600"); runtime.GOOS != "windows" && got != 2 {
		t.Fatalf("expected owner-only PGPASSFILE for both commands, got:\n%s", auditText)
	}
	if strings.Contains(auditText, "pgpass_size=0") || strings.Contains(auditText, "pgpass_size=-1") {
		t.Fatalf("expected populated PGPASSFILE, got:\n%s", auditText)
	}
	if strings.Contains(auditText, "pgpassword=secret") || strings.Contains(auditText, "pgpassword=remote-secret") {
		t.Fatalf("expected PGPASSWORD to be cleared, got:\n%s", auditText)
	}
}

func TestPostgresCommandConnectionSanitizesURLAndEscapesPGPass(t *testing.T) {
	connection, err := parsePostgresCommandConnection("postgres://user:p%3Aass%5Cword@[::1]:5544/app?sslmode=require", api.DBInstance{})
	if err != nil {
		t.Fatal(err)
	}
	if connection.URL != "postgres://[::1]:5544/app?sslmode=require" || connection.URLWithUsername != "postgres://user@[::1]:5544/app?sslmode=require" || connection.User != "user" || connection.Password != `p:ass\word` {
		t.Fatalf("unexpected parsed connection: %+v", connection)
	}
	if got := pgpassEscape(connection.Password); got != `p\:ass\\word` {
		t.Fatalf("escaped pgpass password = %q", got)
	}
}

func TestPostgresURLSanitizationOverridesAmbientCredentialURLs(t *testing.T) {
	t.Setenv("UNRELATED_DATABASE_URL", "postgres://ambient:ambient-secret@db.example/app")
	env := sanitizePostgresURLs(map[string]string{
		"DEV_DATABASE_URL": "postgres://configured:configured-secret@other.example/app",
	})
	if got := env["UNRELATED_DATABASE_URL"]; got != "postgres://db.example/app" {
		t.Fatalf("ambient Postgres URL was not sanitized: %q", got)
	}
	if got := env["DEV_DATABASE_URL"]; got != "postgres://other.example/app" {
		t.Fatalf("configured Postgres URL was not sanitized: %q", got)
	}
}

func TestInvalidPostgresURLDoesNotLeakInputInError(t *testing.T) {
	secretURL := "postgres://user:do-not-log@db.example/%zz"
	_, err := parsePostgresCommandConnection(secretURL, api.DBInstance{})
	if err == nil || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("expected sanitized invalid URL error, got %v", err)
	}
}

func TestPostgresMigrationFileApplierKeepsCredentialsOutOfProcessMetadata(t *testing.T) {
	worktree := t.TempDir()
	binDir := installFakePostgresClients(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	audit := filepath.Join(worktree, "postgres-audit.txt")
	output := filepath.Join(worktree, "applied.sql")
	migrationPath := filepath.Join(worktree, "migrations", "001_init", "up.sql")
	if err := os.MkdirAll(filepath.Dir(migrationPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(migrationPath, []byte("select 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVFLOW_FAKE_PG_AUDIT", audit)
	t.Setenv("DEVFLOW_REMOTE_DATABASE_URL", "postgres://ambient:ambient-secret@remote.example/app")

	db := api.DBInstance{
		Name:     "app_wt_abc",
		URL:      "postgres://devflow:local-secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
		Host:     "127.0.0.1",
		Port:     55432,
		User:     "devflow",
		Password: "local-secret",
	}
	applier := PostgresMigrationFileApplier("up.sql")
	if err := applier(context.Background(), db, MigrationPoint{Name: "001_init"}, MigrationApplyOptions{
		Worktree:      worktree,
		MigrationsDir: "migrations",
		Env: map[string]string{
			"OUT_FILE":         output,
			"DEV_DATABASE_URL": "postgres://configured:configured-secret@remote.example/app",
		},
	}); err != nil {
		t.Fatal(err)
	}

	auditData, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	auditText := string(auditData)
	for _, secret := range []string{"local-secret", "configured-secret", "ambient-secret"} {
		if strings.Contains(auditText, secret) {
			t.Fatalf("database credential %q leaked into psql process metadata: %s", secret, auditText)
		}
	}
	if runtime.GOOS != "windows" && !strings.Contains(auditText, "pgpass_mode=600") {
		t.Fatalf("expected owner-only PGPASSFILE, got:\n%s", auditText)
	}
	urlData, err := os.ReadFile(output + ".url")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(urlData)); got != "postgres://127.0.0.1:55432/app_wt_abc?sslmode=disable" {
		t.Fatalf("psql received unexpected connection URL %q", got)
	}
}

func TestPostgresDumpSourcePolicyContainerStrategyKeepsCredentialsOutOfExecMetadata(t *testing.T) {
	runner := &fakeRunner{}
	engine := &fakeDockerEngine{runner: runner, imageInspects: make(map[string]int)}
	policy := PostgresDumpSourcePolicy{
		RemoteURL:      "postgres://remote-user:remote-secret@db.example:5433/source?sslmode=require",
		ClientStrategy: PostgresClientContainer,
		manager:        newManagerWithDockerEngine(engine),
	}
	db := api.DBInstance{
		Name:          "app_wt_abc",
		URL:           "postgres://devflow:local-secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
		User:          "devflow",
		Password:      "local-secret",
		ContainerPort: 6432,
		ContainerName: "devflow-pg-abc",
	}
	if err := policy.PrepareBase(context.Background(), db, PrepareOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(engine.execs) != 1 {
		t.Fatalf("expected one Docker exec, got %d", len(engine.execs))
	}
	spec := engine.execs[0]
	metadata := strings.Join(append(append([]string(nil), spec.Command...), spec.Env...), "\n")
	for _, secret := range []string{"remote-secret", "local-secret"} {
		if strings.Contains(metadata, secret) {
			t.Fatalf("credential %q leaked into Docker exec command/environment: %s", secret, metadata)
		}
		if !strings.Contains(string(spec.Stdin), secret) {
			t.Fatalf("expected credential %q only in protected stdin payload", secret)
		}
	}
	if !strings.Contains(metadata, "postgres://remote-user@db.example:5433/source?sslmode=require") {
		t.Fatalf("expected sanitized remote URL in exec environment: %s", metadata)
	}
	if !strings.Contains(metadata, "postgres://devflow@127.0.0.1:6432/app_wt_abc?sslmode=disable") {
		t.Fatalf("expected container-local destination URL in exec environment: %s", metadata)
	}
	if !strings.Contains(strings.Join(spec.Command, " "), "umask 077") || !strings.Contains(strings.Join(spec.Command, " "), "trap cleanup") {
		t.Fatalf("expected private temporary files with cleanup, got command: %q", spec.Command)
	}
}

func TestPostgresPassEntryRejectsNewlines(t *testing.T) {
	_, err := postgresPassEntry(postgresCommandConnection{Host: "db.example", User: "user", Password: "secret\ninjected"})
	if err == nil {
		t.Fatal("expected password-file newline to be rejected")
	}
}

func TestPostgresDumpSourcePolicyFailsWhenPgDumpFails(t *testing.T) {
	worktree := t.TempDir()
	binDir := installFakePostgresClients(t)
	psqlMarker := filepath.Join(worktree, "psql-ran.txt")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEVFLOW_FAKE_PG_DUMP_FAIL", "1")
	t.Setenv("DEVFLOW_FAKE_PSQL_RECORD", psqlMarker)
	policy := PostgresDumpSourcePolicy{RemoteURL: "postgres://remote/dev"}
	err := policy.PrepareBase(context.Background(), api.DBInstance{
		Name: "app_wt_abc",
		URL:  "postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
		Host: "127.0.0.1",
		Port: 55432,
		User: "devflow",
	}, PrepareOptions{Worktree: worktree})
	if err == nil {
		t.Fatal("expected pg_dump failure to be returned")
	}
	if _, statErr := os.Stat(psqlMarker); !os.IsNotExist(statErr) {
		t.Fatalf("expected psql not to run after pg_dump failure, stat err=%v", statErr)
	}
}

func installFakePostgresClients(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	source := testutil.BuildTestCommand(t)
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pg_dump", "psql"} {
		target := filepath.Join(binDir, name+testutil.ExeSuffix())
		if err := os.WriteFile(target, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return binDir
}

func TestRuntimePrepareProgressLogsOnceAndEmitsEvent(t *testing.T) {
	worktree := t.TempDir()
	logPath := filepath.Join(worktree, "db_prepare.log")
	var events []string
	rt := &project.Runtime{
		Worktree: worktree,
		LogPath:  logPath,
		Instance: &api.Instance{ID: "abc", Worktree: worktree},
		TaskName: "db_prepare",
		EventFn: func(evt api.Event) {
			if evt.Type == api.EventLogLine {
				events = append(events, evt.Stream+": "+evt.Line)
			}
		},
	}

	emitPrepareLine(PrepareOptionsFromRuntime(rt), "stdout", "database: checking cached Prisma migration snapshots")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	line := "stdout: database: checking cached Prisma migration snapshots"
	if got := strings.Count(string(data), line); got != 1 {
		t.Fatalf("expected one progress log line, got %d in:\n%s", got, string(data))
	}
	if got := strings.Count(strings.Join(events, "\n"), line); got != 1 {
		t.Fatalf("expected one progress event, got %d in %#v", got, events)
	}
}

func TestCommandSourcePolicyRuntimePrepareOptionsDoNotDuplicateProcessLogs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based test is unix-only")
	}
	worktree := t.TempDir()
	logPath := filepath.Join(worktree, "db_prepare.log")
	var events []string
	rt := &project.Runtime{
		Worktree: worktree,
		LogPath:  logPath,
		Instance: &api.Instance{ID: "abc", Worktree: worktree},
		TaskName: "db_prepare",
		EventFn: func(evt api.Event) {
			if evt.Type == api.EventLogLine {
				events = append(events, evt.Stream+": "+evt.Line)
			}
		},
	}
	policy := CommandSourcePolicy{
		PolicyName: "clone-dev",
		Spec: process.CommandSpec{
			Name: "sh",
			Args: []string{"-c", "printf 'hello\\n'"},
		},
	}

	err := policy.PrepareBase(context.Background(), api.DBInstance{
		Name: "app_wt_abc",
		URL:  "postgres://devflow:secret@127.0.0.1:55432/app_wt_abc?sslmode=disable",
		Host: "127.0.0.1",
		Port: 55432,
		User: "devflow",
	}, PrepareOptionsFromRuntime(rt))
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	line := "stdout: hello"
	if got := strings.Count(string(data), line); got != 1 {
		t.Fatalf("expected one subprocess log line, got %d in:\n%s", got, string(data))
	}
	if got := strings.Count(strings.Join(events, "\n"), line); got != 1 {
		t.Fatalf("expected one subprocess event, got %d in %#v", got, events)
	}
}

func mustWriteExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}
