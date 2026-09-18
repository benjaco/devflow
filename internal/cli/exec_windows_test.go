//go:build windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsLocalChildPreservesExitCodeAndResult(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := withEnv(os.Environ(), "DEVFLOW_WINDOWS_CHILD_HELPER", "error")
	var stdout, stderr bytes.Buffer
	err = execLocalBinary(context.Background(), executable,
		[]string{executable, "-test.run=^TestWindowsLocalChildHelper$"}, env, &stdout, &stderr, true, "")
	if ExitCode(err) != 7 {
		t.Fatalf("child exit code = %d, want 7; error=%v", ExitCode(err), err)
	}
	ReportError(&stderr, err)
	if stdout.String() != "{\"success\":false}\n" || stderr.Len() != 0 {
		t.Fatalf("bootstrap duplicated child result: stdout=%s stderr=%s", &stdout, &stderr)
	}
}

func TestWindowsLocalChildCancellationDoesNotDuplicateResult(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	logPath := filepath.Join(t.TempDir(), "tui.log")
	env := withEnv(os.Environ(), "DEVFLOW_WINDOWS_CHILD_HELPER", "wait")
	env = withEnv(env, "DEVFLOW_WINDOWS_CHILD_READY", ready)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	finished := make(chan error, 1)
	go func() {
		finished <- execLocalBinary(ctx, executable,
			[]string{executable, "-test.run=^TestWindowsLocalChildHelper$"}, env, &stdout, &stderr, true, logPath)
	}()
	waitForBootstrapReady(t, ready, finished)
	cancel()
	select {
	case err = <-finished:
	case <-time.After(4 * time.Second):
		t.Fatal("local child did not honor bounded cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("child cancellation lost cause: %v", err)
	}
	var presented interface{ Presented() bool }
	if !errors.As(err, &presented) || !presented.Presented() {
		t.Fatalf("canceled child disowned its emitted JSON: %v", err)
	}
	ReportError(&stderr, err)
	if stdout.String() != "{\"success\":false}\n" || stderr.Len() != 0 {
		t.Fatalf("cancellation duplicated child result: stdout=%s stderr=%s", &stdout, &stderr)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, observed := range []string{"event=bootstrap_parent_canceled", "control=CTRL_BREAK_EVENT", "event=bootstrap_child_exit_observed", "parent_canceled=true"} {
		if !strings.Contains(string(data), observed) {
			t.Fatalf("cancellation lost %q: %s", observed, data)
		}
	}
}

func TestWindowsBootstrapRetainsNativeExitCode(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	logPath := tuiBootstrapLogPath(root, nil)
	env := withEnv(os.Environ(), "DEVFLOW_WINDOWS_CHILD_HELPER", "native-exit")
	var stdout, stderr bytes.Buffer
	err = execLocalBinary(context.Background(), executable,
		[]string{executable, "-test.run=^TestWindowsLocalChildHelper$"}, env, &stdout, &stderr, false, logPath)
	if uint32(ExitCode(err)) != uint32(0xC0000409) || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("native result changed: %v stdout=%s stderr=%s", err, &stdout, &stderr)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, observed := range []string{"event=bootstrap_child_started", "child_pid=", "native_exit=3221226505", "native_hex=0xC0000409", "parent_canceled=false termination_scope=none"} {
		if !strings.Contains(string(data), observed) {
			t.Fatalf("native parent observation lost %q: %s", observed, data)
		}
	}
	t.Logf("native parent evidence:\n%s", data)
}

func TestWindowsLocalChildHelper(t *testing.T) {
	mode := os.Getenv("DEVFLOW_WINDOWS_CHILD_HELPER")
	if mode == "" {
		return
	}
	if mode == "native-exit" {
		os.Exit(int(0xC0000409))
	}
	if mode == "daemon" {
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	if mode == "observer" {
		// Windows leaves CTRL_BREAK to its default handler when no Go signal
		// notification is registered. Consume it so this fixture requires a kill.
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)
		defer signal.Stop(interrupt)
		child := exec.Command(os.Args[0], "-test.run=^TestWindowsLocalChildHelper$")
		child.Env = withEnv(os.Environ(), "DEVFLOW_WINDOWS_CHILD_HELPER", "daemon")
		child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		ready := os.Getenv("DEVFLOW_WINDOWS_CHILD_READY")
		if err := os.WriteFile(ready+".tmp", []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(ready+".tmp", ready); err != nil {
			t.Fatal(err)
		}
		select {
		case <-interrupt:
		case <-time.After(5 * time.Second):
		}
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	fmt.Println(`{"success":false}`)
	if mode == "wait" {
		if err := os.WriteFile(os.Getenv("DEVFLOW_WINDOWS_CHILD_READY"), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		select {
		case <-interrupt:
		case <-time.After(5 * time.Second):
		}
	}
	os.Exit(7)
}

func TestWindowsLocalChildCanceledWithoutResultCannotSucceed(t *testing.T) {
	if err := localChildResult(nil, context.Canceled, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled child without a result = %v, want context.Canceled", err)
	}
	if err := localChildResult(nil, context.Canceled, true); err != nil {
		t.Fatalf("completed child success was replaced after cancellation: %v", err)
	}
}

func TestWindowsLocalChildForcedCancellationPreservesDaemonDescendant(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "daemon-pid")
	logPath := filepath.Join(t.TempDir(), "tui.log")
	env := withEnv(os.Environ(), "DEVFLOW_WINDOWS_CHILD_HELPER", "observer")
	env = withEnv(env, "DEVFLOW_WINDOWS_CHILD_READY", ready)
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	finished := make(chan error, 1)
	go func() {
		finished <- execLocalBinary(ctx, executable,
			[]string{executable, "-test.run=^TestWindowsLocalChildHelper$"}, env, &stdout, &stderr, false, logPath)
		close(finished)
	}()
	t.Cleanup(func() { cancel(); <-finished })
	data := waitForBootstrapReady(t, ready, finished)
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = windows.TerminateProcess(daemon, 1)
		_, _ = windows.WaitForSingleObject(daemon, 2000)
		_ = windows.CloseHandle(daemon)
	})
	cancel()
	select {
	case err = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("local observer did not stop")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("observer cancellation = %v", err)
	}
	log, readErr := os.ReadFile(logPath)
	if readErr != nil || !strings.Contains(string(log), "scope=child_only") || !strings.Contains(string(log), "termination_scope=child_only") {
		t.Fatalf("forced cancellation scope missing: %v\n%s", readErr, log)
	}
	state, err := windows.WaitForSingleObject(daemon, 0)
	if err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("canceling local observer stopped independent daemon: state=%d error=%v", state, err)
	}
}
