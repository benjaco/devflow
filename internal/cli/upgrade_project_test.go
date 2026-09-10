package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

func TestUpgradeProjectConfirmationRequiresYes(t *testing.T) {
	for _, test := range []struct {
		input string
		want  bool
	}{
		{"y\n", true}, {"yes\r\n", true}, {" YES \n", true},
		{"n\n", false}, {"no\n", false}, {"\n", false}, {"maybe\n", false},
		{"", false}, {"yes", false},
	} {
		t.Run(test.input, func(t *testing.T) {
			var output bytes.Buffer
			got, err := confirmProjectUpgrade(context.Background(), strings.NewReader(test.input), &output, "project path", "v1.0.0", "latest")
			if err != nil || got != test.want {
				t.Fatalf("confirmation=%t error=%v; want %t", got, err, test.want)
			}
			for _, want := range []string{"project path", "v1.0.0", "latest", "[y/N]"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("prompt %q missing %q", output.String(), want)
				}
			}
		})
	}
}

func TestUpgradeProjectConfirmationCancellationAndBounds(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prompt := newNotifyingBuffer("[y/N]")
	done := make(chan error, 1)
	go func() {
		_, err := confirmProjectUpgrade(ctx, reader, prompt, "project", "v1.0.0", "latest")
		done <- err
	}()
	select {
	case <-prompt.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt was not written")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("prompt cancellation returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt ignored cancellation")
	}
	confirmed, err := confirmProjectUpgrade(context.Background(), strings.NewReader(strings.Repeat("y", 1024)+"\n"), io.Discard, "project", "v1.0.0", "latest")
	if err == nil || confirmed {
		t.Fatalf("oversized confirmation=%t error=%v", confirmed, err)
	}
}

func TestUpgradeProjectFlagRejectsMissingPinBeforeInstall(t *testing.T) {
	argsPath := installFakeGo(t, 0)
	worktree := t.TempDir()
	writeTestFile(t, filepath.Join(worktree, "go.mod"), "module example.com/app\n\ngo 1.27.1\n")
	var stdout, stderr bytes.Buffer
	app := &App{Stdin: strings.NewReader("yes\n"), Stdout: &stdout, Stderr: &stderr}
	err := app.Run([]string{"upgrade", "--project", "--worktree", worktree, "--json"})
	var result struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil || err == nil || result.Error.Code != "invalid_project_version" {
		t.Fatalf("missing project pin: err=%v decode=%v stdout=%s stderr=%s", err, decodeErr, &stdout, &stderr)
	}
	if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
		t.Fatalf("installer ran before project preflight: %v", err)
	}
}

func TestUpgradeProjectFalseKeepsGlobalRecovery(t *testing.T) {
	argsPath := installFakeGo(t, 0)
	worktree := t.TempDir()
	writeTestFile(t, filepath.Join(worktree, "go.mod"), "broken module\n")
	var stdout, stderr bytes.Buffer
	app := &App{Stdin: strings.NewReader("yes\n"), Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"upgrade", "--project=false", "--worktree", worktree, "--json"}); err != nil {
		t.Fatalf("global recovery inspected project: %v stdout=%s stderr=%s", err, &stdout, &stderr)
	}
	if args, err := os.ReadFile(argsPath); err != nil || !strings.Contains(string(args), "install github.com/benjaco/devflow/cmd/devflow@latest") {
		t.Fatalf("global installer: %q %v", args, err)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "go.mod")); err != nil || string(data) != "broken module\n" {
		t.Fatalf("global upgrade changed broken module: %q %v", data, err)
	}
}

func TestUpgradeProjectReplacementRejectedBeforeInstall(t *testing.T) {
	argsPath := installFakeGo(t, 0)
	worktree := t.TempDir()
	module := "module example.com/app\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v1.0.0\nreplace github.com/benjaco/devflow => example.com/fork v1.1.0\n"
	writeTestFile(t, filepath.Join(worktree, "go.mod"), module)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run([]string{"upgrade", "--project", "--worktree", worktree, "--json"})
	var result api.UpgradeResult
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil || err == nil || result.Error == nil || result.Error.Code != "project_update_failed" || result.Installed {
		t.Fatalf("replacement preflight: %+v err=%v decode=%v stdout=%s", result, err, decodeErr, &stdout)
	}
	if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
		t.Fatalf("replacement rejection ran installer: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "go.mod")); err != nil || string(data) != module {
		t.Fatalf("replacement preflight changed module: %q %v", data, err)
	}
}

func TestUpgradeProjectFailureRetainsInstallationEvidence(t *testing.T) {
	installFakeGo(t, 0)
	worktree := t.TempDir()
	module := "module example.com/app\n\ngo 1.27.1\n\nrequire github.com/benjaco/devflow v1.0.0\n"
	writeTestFile(t, filepath.Join(worktree, "go.mod"), module)
	// The successful fake installer creates no binary. Version inspection must
	// fail without claiming that the global installation or cache clear failed.
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run([]string{"upgrade", "--project", "--worktree", worktree, "--json"})
	var result api.UpgradeResult
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil || err == nil || result.Success || !result.Installed || !result.CacheCleared || result.Error == nil || result.Error.Code != "project_update_failed" {
		t.Fatalf("partial upgrade: %+v err=%v decode=%v stdout=%s stderr=%s", result, err, decodeErr, &stdout, &stderr)
	}
	if result.Project == nil || result.Project.Updated || result.Project.PreviousVersion != "v1.0.0" {
		t.Fatalf("failed project update evidence missing: %+v", result.Project)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "go.mod")); err != nil || string(data) != module {
		t.Fatalf("failed project update changed module: %q %v", data, err)
	}
}

func TestUpgradeHeadlessSelectionNeverReadsInput(t *testing.T) {
	worktree := t.TempDir()
	writeTestFile(t, filepath.Join(worktree, "go.mod"), "module example.com/app\n\ngo 1.27.1\nrequire github.com/benjaco/devflow v1.0.0\n")
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	var output bytes.Buffer
	app := &App{Stdin: reader, Stderr: &output}
	for _, jsonOutput := range []bool{false, true} {
		selection, err := app.selectUpgradeProject(worktree, "latest", nil, jsonOutput)
		if err != nil || selection != nil || output.Len() != 0 {
			t.Fatalf("headless selection prompted: selection=%+v err=%v output=%s", selection, err, &output)
		}
	}
}

func TestUpgradeInstallPathUsesFirstGoPath(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	t.Setenv("DEVFLOW_TEST_GOPATH", first+string(os.PathListSeparator)+second)
	fakeGo := buildFakeGoCommand(t, dir, "env", 0)
	path, err := goInstalledDevflowPath(context.Background(), fakeGo)
	if err != nil || path != filepath.Join(first, "bin", devflowExecutableName()) {
		t.Fatalf("Go install path=%q err=%v", path, err)
	}
}
