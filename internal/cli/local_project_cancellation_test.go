package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/lock"
)

func TestLocalProjectBuildCancellationPreservesPreviousBinary(t *testing.T) {
	worktree := t.TempDir()
	projectPath := filepath.Join(worktree, localProjectFile)
	writeTestFile(t, projectPath, "package main\n")
	ready := installBlockedBootstrapGo(t)
	target := filepath.Join(worktree, ".devflow", "bin", "devflow-local"+testExeSuffix())
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, target, "previous binary")
	writeTestFile(t, localBuildKeyPath(target), "previous-key")
	t.Setenv(envBootstrapModuleVersion, "v0.0.0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- buildLocalProjectBinary(ctx, "", worktree, []string{projectPath}, target, "new-key")
	}()
	waitForBootstrapReady(t, ready, finished)
	started := time.Now()
	cancel()
	err := <-finished
	if !errors.Is(err, context.Canceled) {
		t.Errorf("build cancellation = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("canceled build took %s; build descendants retained output pipes", elapsed)
	}
	for path, want := range map[string]string{target: "previous binary", localBuildKeyPath(target): "previous-key"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Errorf("cancellation changed %s: %q, %v", path, data, err)
		}
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("canceled build retained temporary target: %v", err)
	}
}

func TestLocalProjectBuildLockWaitHonorsCancellation(t *testing.T) {
	worktree := t.TempDir()
	writeTestFile(t, filepath.Join(worktree, localProjectFile), "package main\n")
	held, err := lock.Acquire(localBuildLockPath(worktree))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := ensureLocalProjectBinary(ctx, "", worktree)
		finished <- err
	}()
	select {
	case err := <-finished:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock wait cancellation = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(500 * time.Millisecond):
		_ = held.Release()
		<-finished
		t.Fatal("canceled bootstrap remained blocked on localbuild.lock")
	}
}

func waitForBootstrapReady(t *testing.T, path string, finished <-chan error) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-finished:
			t.Fatal(fmt.Errorf("bootstrap exited before startup marker: %w", err))
		case <-deadline.C:
			t.Fatal("bootstrap did not write startup marker")
		case <-ticker.C:
		}
	}
}

func installBlockedBootstrapGo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "fake-go.go")
	writeTestFile(t, source, `package main
import (
 "os"
 "os/exec"
 "time"
)
func main() {
 if os.Getenv("DEVFLOW_CANCEL_BUILD_CHILD") == "1" {
  time.Sleep(5*time.Second)
  return
 }
 child := exec.Command(os.Args[0])
 child.Env = append(os.Environ(), "DEVFLOW_CANCEL_BUILD_CHILD=1")
 child.Stdout, child.Stderr = os.Stdout, os.Stderr
 if err := child.Start(); err != nil { panic(err) }
 if err := os.WriteFile(os.Getenv("DEVFLOW_CANCEL_BUILD_READY"), []byte("ready"), 0600); err != nil { panic(err) }
 time.Sleep(5*time.Second)
 for i, arg := range os.Args {
  if arg == "-o" { if err := os.WriteFile(os.Args[i+1], []byte("new binary"), 0600); err != nil { panic(err) } }
 }
 _ = child.Wait()
}
`)
	fakeGo := filepath.Join(dir, "go"+testExeSuffix())
	cmd := exec.Command("go", "build", "-o", fakeGo, source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, output)
	}
	ready := filepath.Join(dir, "ready")
	t.Setenv("DEVFLOW_CANCEL_BUILD_READY", ready)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return ready
}
