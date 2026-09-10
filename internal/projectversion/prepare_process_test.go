package projectversion

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Copying this native Go executable supplies a portable build subprocess,
	// including on Windows, without shell scripts or another compiler invocation.
	if len(os.Args) > 1 && os.Args[1] == "build" {
		switch os.Getenv("DEVFLOW_PROJECTVERSION_BUILD_HELPER") {
		case "fail":
			fmt.Fprint(os.Stderr, strings.Repeat("build output\n", 4000), "final compiler diagnostic\n")
			os.Exit(9)
		case "pause":
			if err := os.WriteFile(os.Getenv("DEVFLOW_PROJECTVERSION_ENTERED"), []byte("entered"), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			for {
				if _, err := os.Stat(os.Getenv("DEVFLOW_PROJECTVERSION_RELEASE")); err == nil {
					os.Exit(0)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
	os.Exit(m.Run())
}

func TestPrepareBuildFailureCancellationAndSelectionEdit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(tools, name), data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))

	t.Run("compiler failure retains bounded diagnostic", func(t *testing.T) {
		t.Setenv("DEVFLOW_PROJECTVERSION_BUILD_HELPER", "fail")
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/app\nrequire github.com/benjaco/devflow v1.2.3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		selected, err := Resolve(root)
		if err != nil {
			t.Fatal(err)
		}
		var progress bytes.Buffer
		binary, err := Prepare(context.Background(), selected, &progress)
		if err == nil || binary != "" || !strings.Contains(err.Error(), "final compiler diagnostic") || len(err.Error()) > diagnosticLimit+200 {
			t.Fatalf("compiler failure lost bounded diagnostics: binary=%q, err=%v", binary, err)
		}
		if progress.Len() < diagnosticLimit*2 {
			t.Fatalf("live build output was truncated to retained diagnostic: %d bytes", progress.Len())
		}
		if _, err := os.Stat(BinaryPath(selected)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed compilation published a binary: %v", err)
		}
	})

	for _, operation := range []string{"cancel", "edit selection"} {
		t.Run(operation, func(t *testing.T) {
			t.Setenv("DEVFLOW_PROJECTVERSION_BUILD_HELPER", "pause")
			root := t.TempDir()
			entered := filepath.Join(root, "build-entered")
			release := filepath.Join(root, "build-release")
			t.Setenv("DEVFLOW_PROJECTVERSION_ENTERED", entered)
			t.Setenv("DEVFLOW_PROJECTVERSION_RELEASE", release)
			module := []byte("module example.test/app\nrequire github.com/benjaco/devflow v1.2.3\n")
			if err := os.WriteFile(filepath.Join(root, "go.mod"), module, 0o600); err != nil {
				t.Fatal(err)
			}
			selected, err := Resolve(root)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			var binary string
			var buildErr error
			go func() {
				defer close(done)
				binary, buildErr = Prepare(ctx, selected, io.Discard)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("build subprocess did not exit after cancellation")
				}
			})
			watchdog := time.NewTimer(10 * time.Second)
			defer watchdog.Stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
		waiting:
			for {
				if _, err := os.Stat(entered); err == nil {
					break waiting
				}
				select {
				case <-done:
					t.Fatalf("build exited before handshake: %v", buildErr)
				case <-watchdog.C:
					t.Fatal("build subprocess did not reach handshake")
				case <-ticker.C:
				}
			}
			if operation == "cancel" {
				cancel()
			} else {
				if err := os.WriteFile(filepath.Join(root, "go.mod"), bytes.ReplaceAll(module, []byte("v1.2.3"), []byte("v1.2.4")), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("prepare did not finish after cancellation or release")
			}
			if buildErr == nil || binary != "" {
				t.Fatalf("interrupted preparation succeeded: %s, %v", binary, buildErr)
			}
			if operation == "cancel" && !errors.Is(buildErr, context.Canceled) {
				t.Fatalf("build cancellation lost its cause: %v", buildErr)
			}
			if operation == "edit selection" && !strings.Contains(buildErr.Error(), "selection changed") {
				t.Fatalf("build did not reject edited selection: %v", buildErr)
			}
			if _, err := os.Stat(BinaryPath(selected)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("interrupted preparation published a binary: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(root, ".devflow", "versions"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "prepare.lock" {
				t.Fatalf("interrupted preparation left staging artifacts: %v", entries)
			}
		})
	}
}
