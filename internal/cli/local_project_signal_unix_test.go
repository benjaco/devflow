//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

func TestBootstrapInterruptReturnsOneJSONError(t *testing.T) {
	binary := buildBootstrapBinary(t)
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	writeTestFile(t, filepath.Join(worktree, localProjectFile), "package main\n")
	ready := installBlockedBootstrapGo(t)
	cmd := exec.Command(binary, "graph", "list", "--json")
	cmd.Dir = worktree
	cmd.Env = withEnv(os.Environ(), envBootstrapRoot, root)
	cmd.Env = withEnv(cmd.Env, envBootstrapEntry, "1")
	cmd.Env = withEnv(cmd.Env, envLocalExec, "")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if err != nil {
			err = fmt.Errorf("%w; stdout=%s; stderr=%s", err, &stdout, &stderr)
		}
		finished <- err
	}()
	waitForBootstrapReady(t, ready, finished)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("interrupted bootstrap exited successfully")
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-finished
		t.Fatal("interrupt failed to stop bootstrap")
	}
	var result struct {
		Success bool              `json:"success"`
		Error   *api.CommandError `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("expected exactly one JSON error: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	if result.Success || result.Error == nil || result.Error.Code != "operation_cancelled" || result.Error.Phase != "bootstrap" {
		t.Fatalf("bootstrap interruption result: %+v", result)
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON bootstrap duplicated error on stderr: %s", &stderr)
	}
}
