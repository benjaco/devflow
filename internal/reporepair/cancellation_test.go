package reporepair

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRepositoryGitCancellationPreservesCauseAndStopsDescendants(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, outputProbe := range []bool{false, true} {
		name := "captured output"
		if outputProbe {
			name = "presence probe"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			ready := filepath.Join(root, "ready")
			t.Setenv("DEVFLOW_REPAIR_CANCEL_HELPER", "parent")
			t.Setenv("DEVFLOW_REPAIR_CANCEL_READY", ready)
			runner := &Runner{root: root, gitPath: executable}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				var err error
				if outputProbe {
					_, err = runner.gitProducesOutput(ctx, "-test.run=^TestRepositoryGitCancellationHelper$")
				} else {
					_, err = runner.git(ctx, nil, nil, "-test.run=^TestRepositoryGitCancellationHelper$")
				}
				finished <- err
			}()
			deadline := time.NewTimer(10 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				select {
				case err := <-finished:
					t.Fatalf("Git helper exited before descendant startup: %v", err)
				case <-deadline.C:
					t.Fatal("Git helper did not start descendant")
				case <-ticker.C:
				}
			}
			started := time.Now()
			cancel()
			if err := <-finished; !errors.Is(err, context.Canceled) {
				t.Errorf("Git cancellation lost its cause: %v", err)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Errorf("Git cancellation took %s; descendants retained output pipes", elapsed)
			}
		})
	}
}

func TestRepositoryGitCancellationHelper(t *testing.T) {
	switch os.Getenv("DEVFLOW_REPAIR_CANCEL_HELPER") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestRepositoryGitCancellationHelper$")
		child.Env = append(os.Environ(), "DEVFLOW_REPAIR_CANCEL_HELPER=child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		_ = child.Wait()
	case "child":
		if err := os.WriteFile(os.Getenv("DEVFLOW_REPAIR_CANCEL_READY"), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
	}
}
