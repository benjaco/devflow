//go:build windows

package daemon

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsDaemonSurvivesClientConsoleInterruption(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "daemon-pid")
	launcher := exec.Command(executable, "-test.run=^TestWindowsDaemonConsoleHelper$")
	launcher.Env = append(os.Environ(), "DEVFLOW_DAEMON_CONSOLE_HELPER=launcher", "DEVFLOW_DAEMON_CONSOLE_READY="+ready)
	launcher.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if err := launcher.Start(); err != nil {
		t.Fatal(err)
	}
	defer launcher.Process.Kill()
	finished := make(chan error, 1)
	go func() { finished <- launcher.Wait() }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	var data []byte
	for {
		if data, err = os.ReadFile(ready); err == nil {
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("launcher ended before daemon startup: %v", err)
		case <-deadline.C:
			t.Fatal("launcher did not start daemon")
		case <-ticker.C:
		}
	}
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
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(launcher.Process.Pid)); err != nil {
		t.Skipf("console control unavailable: %v", err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("launcher did not handle its interrupt: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client group did not receive interruption")
	}
	state, err := windows.WaitForSingleObject(daemon, 100)
	if err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("daemon received client console interruption: state=%d error=%v", state, err)
	}
}

func TestWindowsDaemonConsoleHelper(t *testing.T) {
	mode := os.Getenv("DEVFLOW_DAEMON_CONSOLE_HELPER")
	if mode == "" {
		return
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	if mode == "launcher" {
		child := exec.Command(os.Args[0], "-test.run=^TestWindowsDaemonConsoleHelper$")
		child.Env = append(os.Environ(), "DEVFLOW_DAEMON_CONSOLE_HELPER=daemon")
		prepareDaemonCmd(child)
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		ready := os.Getenv("DEVFLOW_DAEMON_CONSOLE_READY")
		if err := os.WriteFile(ready+".tmp", []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(ready+".tmp", ready); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-interrupt:
	case <-time.After(15 * time.Second):
	}
	os.Exit(0)
}
