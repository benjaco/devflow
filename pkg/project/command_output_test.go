package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/testutil"
	"github.com/benjaco/devflow/pkg/process"
)

func TestCommandOutputTaskletRetriesSuccessfulCommandUntilFilesExist(t *testing.T) {
	worktree := t.TempDir()
	counter := filepath.Join(worktree, ".attempts")
	tasklet := CommandOutputTasklet{
		Command: process.CommandSpec{
			Name: testutil.BuildTestCommand(t),
			Args: []string{"write-after", counter, "2", "generated/result.txt", "ready"},
		},
		RequiredFiles: []string{"generated/result.txt"},
		MaxAttempts:   3,
		RetryDelay:    time.Millisecond,
	}

	if err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree}); err != nil {
		t.Fatal(err)
	}
	if got := readCommandOutputTestFile(t, counter); got != "2" {
		t.Fatalf("command attempts = %q, want 2", got)
	}
	if got := readCommandOutputTestFile(t, filepath.Join(worktree, "generated", "result.txt")); got != "ready" {
		t.Fatalf("output = %q, want ready", got)
	}
}

func TestCommandOutputTaskletRequiresNewGlobMatch(t *testing.T) {
	worktree := t.TempDir()
	writeCommandOutputTestFile(t, filepath.Join(worktree, "migrations", "old.ts"), "old")
	tasklet := CommandOutputTasklet{
		Command: process.CommandSpec{
			Name: testutil.BuildTestCommand(t),
			Args: []string{"write", "migrations/new.ts", "new"},
		},
		RequiredFiles:   []string{"migrations/**/*.ts"},
		RequireNewFiles: true,
		MaxAttempts:     1,
	}

	if err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree}); err != nil {
		t.Fatal(err)
	}
}

func TestCommandOutputTaskletRejectsOnlyPreexistingGlobMatches(t *testing.T) {
	worktree := t.TempDir()
	writeCommandOutputTestFile(t, filepath.Join(worktree, "migrations", "old.ts"), "old")
	tasklet := CommandOutputTasklet{
		Command: process.CommandSpec{
			Name: testutil.BuildTestCommand(t),
			Args: []string{"emit", "success", ""},
		},
		RequiredFiles:   []string{"migrations/**/*.ts"},
		RequireNewFiles: true,
		MaxAttempts:     2,
		RetryDelay:      time.Millisecond,
	}

	err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree})
	if err == nil || !strings.Contains(err.Error(), "matched no newly created files") {
		t.Fatalf("expected missing new file error, got %v", err)
	}
}

func TestCommandOutputTaskletCleansOutputDirectoriesOnceBeforeRunning(t *testing.T) {
	worktree := t.TempDir()
	writeCommandOutputTestFile(t, filepath.Join(worktree, "generated", "stale.txt"), "stale")
	writeCommandOutputTestFile(t, filepath.Join(worktree, "generated", "result.txt"), "old")
	writeCommandOutputTestFile(t, filepath.Join(worktree, ".generated.hash"), "stale-hash")
	tasklet := CommandOutputTasklet{
		Command: process.CommandSpec{
			Name: testutil.BuildTestCommand(t),
			Args: []string{"write-if-missing", "generated", ".generated.hash", "generated/result.txt", "fresh"},
		},
		RequiredFiles:   []string{"generated/**/*"},
		OutputDirs:      []string{"generated"},
		OutputHashFiles: []string{".generated.hash"},
		CleanOutputDirs: true,
		RequireNewFiles: true,
		MaxAttempts:     1,
	}

	if err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "generated", "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale output should have been removed, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".generated.hash")); !os.IsNotExist(err) {
		t.Fatalf("stale output hash should have been removed, stat error = %v", err)
	}
	if got := readCommandOutputTestFile(t, filepath.Join(worktree, "generated", "result.txt")); got != "fresh" {
		t.Fatalf("output = %q, want fresh", got)
	}
}

func TestCommandOutputTaskletValidatesHashFilesBeforeCleaningAnything(t *testing.T) {
	worktree := t.TempDir()
	sentinel := filepath.Join(worktree, "generated", "keep.txt")
	writeCommandOutputTestFile(t, sentinel, "keep")
	tasklet := CommandOutputTasklet{
		Command:         process.CommandSpec{Name: testutil.BuildTestCommand(t), Args: []string{"emit", "", ""}},
		RequiredFiles:   []string{"generated/result.txt"},
		OutputDirs:      []string{"generated"},
		OutputHashFiles: []string{"../outside.hash"},
		CleanOutputDirs: true,
		MaxAttempts:     1,
	}

	err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree})
	if err == nil || !strings.Contains(err.Error(), "must stay inside the worktree") {
		t.Fatalf("expected unsafe hash cleanup error, got %v", err)
	}
	if got := readCommandOutputTestFile(t, sentinel); got != "keep" {
		t.Fatalf("output was cleaned before hash validation failed: %q", got)
	}
}

func TestCommandOutputTaskletRejectsHashFilesWithoutOutputCleanup(t *testing.T) {
	worktree := t.TempDir()
	hashFile := filepath.Join(worktree, ".generated.hash")
	writeCommandOutputTestFile(t, hashFile, "keep")
	tasklet := CommandOutputTasklet{
		Command:         process.CommandSpec{Name: testutil.BuildTestCommand(t), Args: []string{"emit", "", ""}},
		RequiredFiles:   []string{"generated/result.txt"},
		OutputHashFiles: []string{".generated.hash"},
		MaxAttempts:     1,
	}

	err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree})
	if err == nil || !strings.Contains(err.Error(), "require output-directory cleanup") {
		t.Fatalf("expected hash cleanup mode error, got %v", err)
	}
	if got := readCommandOutputTestFile(t, hashFile); got != "keep" {
		t.Fatalf("hash was cleaned without output cleanup: %q", got)
	}
}

func TestCommandOutputTaskletDoesNotCleanOutputDirectoryBetweenRetries(t *testing.T) {
	worktree := t.TempDir()
	counter := filepath.Join(worktree, "generated", ".attempts")
	tasklet := CommandOutputTasklet{
		Command: process.CommandSpec{
			Name: testutil.BuildTestCommand(t),
			Args: []string{"write-after", counter, "2", "generated/result.txt", "ready"},
		},
		RequiredFiles:   []string{"generated/result.txt"},
		OutputDirs:      []string{"generated"},
		CleanOutputDirs: true,
		MaxAttempts:     2,
		RetryDelay:      time.Millisecond,
	}

	if err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree}); err != nil {
		t.Fatal(err)
	}
	if got := readCommandOutputTestFile(t, counter); got != "2" {
		t.Fatalf("counter was cleaned between retries; attempts = %q", got)
	}
}

func TestCommandOutputTaskletWaitsForDelayedFilesAfterSuccessfulExit(t *testing.T) {
	worktree := t.TempDir()
	output := filepath.Join(worktree, "generated", "result.txt")
	tasklet := CommandOutputTasklet{
		Command: process.CommandSpec{
			Name: testutil.BuildTestCommand(t),
			Args: []string{"emit", "success", ""},
		},
		RequiredFiles: []string{"generated/result.txt"},
		MaxAttempts:   1,
		RetryDelay:    150 * time.Millisecond,
	}
	done := make(chan error, 1)
	go func() {
		time.Sleep(25 * time.Millisecond)
		if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
			done <- err
			return
		}
		done <- os.WriteFile(output, []byte("delayed"), 0o644)
	}()

	if err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCommandOutputTaskletValidatesAllCleanupPathsBeforeDeleting(t *testing.T) {
	worktree := t.TempDir()
	sentinel := filepath.Join(worktree, "generated", "keep.txt")
	writeCommandOutputTestFile(t, sentinel, "keep")
	tasklet := CommandOutputTasklet{
		Command:         process.CommandSpec{Name: testutil.BuildTestCommand(t), Args: []string{"emit", "", ""}},
		RequiredFiles:   []string{"generated/result.txt"},
		OutputDirs:      []string{"generated", "../outside"},
		CleanOutputDirs: true,
		MaxAttempts:     1,
	}

	err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree})
	if err == nil || !strings.Contains(err.Error(), "must stay inside the worktree") {
		t.Fatalf("expected unsafe cleanup error, got %v", err)
	}
	if got := readCommandOutputTestFile(t, sentinel); got != "keep" {
		t.Fatalf("validated cleanup removed sentinel before failing: %q", got)
	}
}

func TestCommandOutputTaskletRejectsMalformedGlobBeforeRunning(t *testing.T) {
	worktree := t.TempDir()
	tasklet := CommandOutputTasklet{
		Command:       process.CommandSpec{Name: testutil.BuildTestCommand(t), Args: []string{"write", "ran.txt", "ran"}},
		RequiredFiles: []string{"generated/[.ts"},
		MaxAttempts:   1,
	}

	err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree})
	if err == nil || !strings.Contains(err.Error(), "malformed glob pattern") {
		t.Fatalf("expected malformed glob error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "ran.txt")); !os.IsNotExist(err) {
		t.Fatalf("command ran before glob validation, stat error = %v", err)
	}
}

func TestCommandOutputTaskletRejectsReservedCleanupDirectory(t *testing.T) {
	worktree := t.TempDir()
	sentinel := filepath.Join(worktree, ".devflow", "keep.txt")
	writeCommandOutputTestFile(t, sentinel, "keep")
	tasklet := CommandOutputTasklet{
		Command:         process.CommandSpec{Name: testutil.BuildTestCommand(t), Args: []string{"emit", "", ""}},
		RequiredFiles:   []string{".devflow/generated/result.txt"},
		OutputDirs:      []string{".devflow/generated"},
		CleanOutputDirs: true,
		MaxAttempts:     1,
	}

	err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree})
	if err == nil || !strings.Contains(err.Error(), "state directories cannot be cleaned") {
		t.Fatalf("expected reserved cleanup error, got %v", err)
	}
	if got := readCommandOutputTestFile(t, sentinel); got != "keep" {
		t.Fatalf("reserved state changed: %q", got)
	}
}

func TestCommandOutputTaskletDoesNotCleanWhenContextIsAlreadyCanceled(t *testing.T) {
	worktree := t.TempDir()
	sentinel := filepath.Join(worktree, "generated", "keep.txt")
	writeCommandOutputTestFile(t, sentinel, "keep")
	tasklet := CommandOutputTasklet{
		Command:         process.CommandSpec{Name: testutil.BuildTestCommand(t), Args: []string{"emit", "", ""}},
		RequiredFiles:   []string{"generated/result.txt"},
		OutputDirs:      []string{"generated"},
		CleanOutputDirs: true,
		MaxAttempts:     1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := tasklet.Run(ctx, &Runtime{Worktree: worktree})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled error, got %v", err)
	}
	if got := readCommandOutputTestFile(t, sentinel); got != "keep" {
		t.Fatalf("canceled tasklet cleaned outputs: %q", got)
	}
}

func TestCommandOutputTaskletRejectsFileCleanupTarget(t *testing.T) {
	worktree := t.TempDir()
	sentinel := filepath.Join(worktree, "generated")
	writeCommandOutputTestFile(t, sentinel, "keep")
	tasklet := CommandOutputTasklet{
		Command:         process.CommandSpec{Name: testutil.BuildTestCommand(t), Args: []string{"emit", "", ""}},
		RequiredFiles:   []string{"generated/result.txt"},
		OutputDirs:      []string{"generated"},
		CleanOutputDirs: true,
		MaxAttempts:     1,
	}

	err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree})
	if err == nil || !strings.Contains(err.Error(), "cleanup target is not a directory") {
		t.Fatalf("expected file cleanup target error, got %v", err)
	}
	if got := readCommandOutputTestFile(t, sentinel); got != "keep" {
		t.Fatalf("file cleanup target changed: %q", got)
	}
}

func TestCommandOutputTaskletRejectsSymlinkCleanupTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires privileges on many Windows hosts")
	}
	worktree := t.TempDir()
	external := t.TempDir()
	sentinel := filepath.Join(external, "keep.txt")
	writeCommandOutputTestFile(t, sentinel, "keep")
	if err := os.Symlink(external, filepath.Join(worktree, "generated")); err != nil {
		t.Fatal(err)
	}
	tasklet := CommandOutputTasklet{
		Command:         process.CommandSpec{Name: testutil.BuildTestCommand(t), Args: []string{"emit", "", ""}},
		RequiredFiles:   []string{"generated/result.txt"},
		OutputDirs:      []string{"generated"},
		CleanOutputDirs: true,
		MaxAttempts:     1,
	}

	err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree})
	if err == nil || !strings.Contains(err.Error(), "traverses symlink") {
		t.Fatalf("expected symlink cleanup error, got %v", err)
	}
	if got := readCommandOutputTestFile(t, sentinel); got != "keep" {
		t.Fatalf("external sentinel changed: %q", got)
	}
}

func TestCommandOutputTaskletRejectsSymlinkedHashFileParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires privileges on many Windows hosts")
	}
	worktree := t.TempDir()
	external := t.TempDir()
	externalHash := filepath.Join(external, "generated.hash")
	writeCommandOutputTestFile(t, externalHash, "keep")
	outputSentinel := filepath.Join(worktree, "generated", "keep.txt")
	writeCommandOutputTestFile(t, outputSentinel, "keep")
	if err := os.Symlink(external, filepath.Join(worktree, "hashes")); err != nil {
		t.Fatal(err)
	}
	tasklet := CommandOutputTasklet{
		Command:         process.CommandSpec{Name: testutil.BuildTestCommand(t), Args: []string{"emit", "", ""}},
		RequiredFiles:   []string{"generated/result.txt"},
		OutputDirs:      []string{"generated"},
		OutputHashFiles: []string{"hashes/generated.hash"},
		CleanOutputDirs: true,
		MaxAttempts:     1,
	}

	err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree})
	if err == nil || !strings.Contains(err.Error(), "traverses symlink") {
		t.Fatalf("expected hash symlink cleanup error, got %v", err)
	}
	if got := readCommandOutputTestFile(t, outputSentinel); got != "keep" {
		t.Fatalf("output was cleaned before hash validation failed: %q", got)
	}
	if got := readCommandOutputTestFile(t, externalHash); got != "keep" {
		t.Fatalf("external hash changed: %q", got)
	}
}

func TestCommandOutputTaskletStopsAfterConfiguredAttempts(t *testing.T) {
	worktree := t.TempDir()
	counter := filepath.Join(worktree, ".attempts")
	tasklet := CommandOutputTasklet{
		Command: process.CommandSpec{
			Name: testutil.BuildTestCommand(t),
			Args: []string{"write-after", counter, "99", "generated/result.txt", "ready"},
		},
		RequiredFiles: []string{"generated/result.txt"},
		MaxAttempts:   2,
		RetryDelay:    time.Millisecond,
	}

	err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree})
	if err == nil || !strings.Contains(err.Error(), "completed successfully 2 times") {
		t.Fatalf("expected exhausted-attempt error, got %v", err)
	}
	if got := readCommandOutputTestFile(t, counter); got != "2" {
		t.Fatalf("command attempts = %q, want 2", got)
	}
}

func TestCommandOutputTaskletDoesNotRetryFailedCommand(t *testing.T) {
	worktree := t.TempDir()
	counter := filepath.Join(worktree, ".attempts")
	if err := os.Mkdir(filepath.Join(worktree, "cannot-write-file"), 0o755); err != nil {
		t.Fatal(err)
	}
	tasklet := CommandOutputTasklet{
		Command: process.CommandSpec{
			Name: testutil.BuildTestCommand(t),
			Args: []string{"write-after", counter, "1", "cannot-write-file", "ready"},
		},
		RequiredFiles: []string{"generated/result.txt"},
		MaxAttempts:   3,
		RetryDelay:    time.Millisecond,
	}

	err := tasklet.Run(context.Background(), &Runtime{Worktree: worktree})
	if err == nil || !strings.Contains(err.Error(), "exited with code 1") {
		t.Fatalf("expected immediate command failure, got %v", err)
	}
	if got := readCommandOutputTestFile(t, counter); got != "1" {
		t.Fatalf("failed command attempts = %q, want 1", got)
	}
}

func TestCommandOutputTaskletHonorsCancellationBetweenAttempts(t *testing.T) {
	worktree := t.TempDir()
	tasklet := CommandOutputTasklet{
		Command: process.CommandSpec{
			Name: testutil.BuildTestCommand(t),
			Args: []string{"emit", "success", ""},
		},
		RequiredFiles: []string{"generated/result.txt"},
		MaxAttempts:   5,
		RetryDelay:    time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := tasklet.Run(ctx, &Runtime{Worktree: worktree})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}

func readCommandOutputTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeCommandOutputTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
