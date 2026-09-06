package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRunCancellationStopsDescendantsHoldingOutput(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := Run(ctx, CommandSpec{
			Name: executable,
			Args: []string{"-test.run=^TestRunCancellationProcessHelper$"},
			Env:  map[string]string{"DEVFLOW_PROCESS_CANCEL_HELPER": "parent", "DEVFLOW_PROCESS_CANCEL_READY": ready},
		})
		finished <- err
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("command ended before descendant startup: %v", err)
		case <-deadline.C:
			t.Fatal("command did not start descendant")
		case <-ticker.C:
		}
	}
	started := time.Now()
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Errorf("cancellation = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("canceled command took %s; descendants retained output pipes", elapsed)
	}
}

func TestRunCancellationProcessHelper(t *testing.T) {
	switch os.Getenv("DEVFLOW_PROCESS_CANCEL_HELPER") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestRunCancellationProcessHelper$")
		child.Env = append(os.Environ(), "DEVFLOW_PROCESS_CANCEL_HELPER=child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		_ = child.Wait()
	case "child":
		if err := os.WriteFile(os.Getenv("DEVFLOW_PROCESS_CANCEL_READY"), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
	}
}
