package project

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/testutil"
	"github.com/benjaco/devflow/pkg/api"
)

func TestGoDebugServiceWaitsForDebuggerInitialization(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	task := GoDebugService("debug", GoDebugServiceOptions{DebugPortName: "debug"})
	rt := &Runtime{Instance: &api.Instance{Ports: map[string]int{"debug": port}}}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	// Like Delve during launch, this listener can complete TCP handshakes but
	// cannot yet answer debugger requests.
	if err := task.Ready(ctx, rt); err == nil {
		t.Fatal("debug service became ready before the debugger initialized")
	}
}

func TestGoDebugServiceReadyCancellationClosesRPC(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	task := GoDebugService("debug", GoDebugServiceOptions{DebugPortName: "debug"})
	rt := &Runtime{Instance: &api.Instance{Ports: map[string]int{"debug": port}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- task.Ready(ctx, rt) }()
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("readiness error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled readiness left a blocked RPC")
	}
}

func TestGoDebugServiceStopsThroughDebuggerDuringStartup(t *testing.T) {
	binary, err := os.ReadFile(testutil.BuildTestCommand(t))
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	for _, name := range []string{"go", "dlv"} {
		if err := os.WriteFile(filepath.Join(binDir, name+testutil.ExeSuffix()), binary, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, mode := range []string{"stop", "cancel", "concurrent", "resume", "stalled"} {
		t.Run(mode, func(t *testing.T) {
			worktree := t.TempDir()
			record := filepath.Join(worktree, "debug-record")
			gate := filepath.Join(worktree, "initialized")
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			_ = listener.Close()
			listening := make(chan struct{}, 1)
			var handle ServiceHandle
			rt := &Runtime{
				Worktree: worktree, TaskName: "debug", LogPath: filepath.Join(worktree, "debug.log"),
				Instance: &api.Instance{Ports: map[string]int{"debug": port}},
				Env: map[string]string{
					"DEVFLOW_FAKE_DEBUG_RECORD":    record,
					"DEVFLOW_FAKE_DEBUG_INIT_FILE": gate,
				},
				OnServiceHandle: func(_ string, h ServiceHandle) { handle = h },
				EventFn: func(event api.Event) {
					if strings.Contains(event.Line, "API server listening at:") {
						listening <- struct{}{}
					}
				},
			}
			if mode == "resume" {
				rt.Env["DEVFLOW_FAKE_DEBUG_RESUME_AFTER_HALT"] = "1"
			}
			task := GoDebugService("debug", GoDebugServiceOptions{DebugPortName: "debug", StopGrace: 2 * time.Second})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := task.Run(ctx, rt); err != nil {
				t.Fatal(err)
			}
			defer handle.Stop()
			select {
			case <-listening:
			case <-time.After(5 * time.Second):
				t.Fatal("debugger did not open its listener")
			}
			stopped := make(chan error, 1)
			if mode == "cancel" || mode == "concurrent" {
				cancel()
			}
			go func() {
				if mode == "cancel" {
					stopped <- handle.Wait()
				} else {
					stopped <- handle.Stop()
				}
			}()
			if mode != "stalled" {
				if err := os.WriteFile(gate, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("debugger shutdown did not finish")
			}
			if handle.Alive() {
				t.Fatal("debugger remains alive after stop")
			}
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if mode == "stalled" {
				want = 0 // RPC never starts; the bounded OS fallback must reap it.
			}
			if got := strings.Count(string(data), "fake-dlv detach kill=true"); got != want {
				t.Fatalf("debugger kill/detach requests = %d, want %d; record:\n%s", got, want, data)
			}
		})
	}
}
