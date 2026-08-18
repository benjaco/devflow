package database

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

type runtimeServiceEngine struct {
	dockerEngine
	db              api.DBInstance
	watchStarted    chan struct{}
	watchExitOnStop chan struct{}
	stopCalls       atomic.Int32
}

type runtimeStopEngine struct {
	dockerEngine
	exists    bool
	running   bool
	stopCalls atomic.Int32
	stopErr   error
}

func (e *runtimeStopEngine) InspectContainer(context.Context, string) (dockerContainer, bool, error) {
	return dockerContainer{Running: e.running}, e.exists, nil
}

func (e *runtimeStopEngine) StopContainer(context.Context, string, int) error {
	e.stopCalls.Add(1)
	return e.stopErr
}

func TestStopRuntimeIfRunningReportsOnlyPhysicalStop(t *testing.T) {
	for _, test := range []struct {
		name        string
		exists      bool
		running     bool
		wantStopped bool
		wantCalls   int32
	}{
		{name: "missing", exists: false},
		{name: "already-stopped", exists: true},
		{name: "running", exists: true, running: true, wantStopped: true, wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := &runtimeStopEngine{exists: test.exists, running: test.running}
			manager := newManagerWithDockerEngine(engine)
			stopped, err := manager.StopRuntimeIfRunning(context.Background(), api.DBInstance{ContainerName: "devflow-pg-test"})
			if err != nil {
				t.Fatal(err)
			}
			if stopped != test.wantStopped || engine.stopCalls.Load() != test.wantCalls {
				t.Fatalf("stopped=%v calls=%d, want stopped=%v calls=%d", stopped, engine.stopCalls.Load(), test.wantStopped, test.wantCalls)
			}
		})
	}
}

func (e *runtimeServiceEngine) InspectContainer(context.Context, string) (dockerContainer, bool, error) {
	return dockerContainer{
		Running: true,
		Image:   e.db.Image,
		Ports:   map[int][]int{dbContainerPort(e.db): {e.db.Port}},
		Mounts: []dockerMount{{
			Type:        "volume",
			Name:        e.db.VolumeName,
			Destination: postgresVolumeMount(e.db),
		}},
	}, true, nil
}

func (e *runtimeServiceEngine) WatchContainer(ctx context.Context, _ string, onLine func(string, string)) error {
	if onLine != nil {
		onLine("stdout", "database ready")
	}
	close(e.watchStarted)
	if e.watchExitOnStop != nil {
		<-e.watchExitOnStop
		return &dockerContainerExitError{name: e.db.ContainerName, code: 137}
	}
	<-ctx.Done()
	return ctx.Err()
}

func (e *runtimeServiceEngine) StopContainer(context.Context, string, int) error {
	e.stopCalls.Add(1)
	if e.watchExitOnStop != nil {
		close(e.watchExitOnStop)
	}
	return nil
}

func TestRuntimeServiceHandleSupervisesContainerWithoutProcess(t *testing.T) {
	db := api.DBInstance{
		Name:          "app",
		Port:          55432,
		ContainerPort: DefaultContainerPort,
		User:          "devflow",
		Password:      "devflow",
		Image:         DefaultPostgresImage,
		ContainerName: "devflow-pg-service",
		VolumeName:    "devflow-pgdata-service",
	}
	line := make(chan string, 1)
	engine := &runtimeServiceEngine{
		db:           db,
		watchStarted: make(chan struct{}),
	}
	manager := newManagerWithDockerEngine(engine)
	handle, err := manager.StartRuntimeService(context.Background(), db, RuntimeServiceOptions{
		OnLine: func(stream, value string) {
			line <- stream + ":" + value
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-engine.watchStarted:
	case <-time.After(time.Second):
		t.Fatal("container watch did not start")
	}
	if handle.PID() != 0 {
		t.Fatalf("managed container service PID = %d, want zero", handle.PID())
	}
	if !handle.Alive() {
		t.Fatal("managed container service should be alive")
	}
	select {
	case got := <-line:
		if got != "stdout:database ready" {
			t.Fatalf("forwarded log line = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("managed container log was not forwarded")
	}
	if err := handle.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Wait(); err != nil {
		t.Fatalf("intentional managed-container stop returned %v", err)
	}
	if handle.Alive() {
		t.Fatal("managed container service remained alive after stop")
	}
	if got := engine.stopCalls.Load(); got != 1 {
		t.Fatalf("container stop calls = %d, want 1", got)
	}
	if err := handle.Stop(); err != nil {
		t.Fatal(err)
	}
	if got := engine.stopCalls.Load(); got != 1 {
		t.Fatalf("idempotent stop calls = %d, want 1", got)
	}
}

func TestZeroRuntimeServiceHandleIsInert(t *testing.T) {
	handle := &RuntimeServiceHandle{}
	if handle.Alive() {
		t.Fatal("zero runtime service handle must not be alive")
	}
	if err := handle.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeServiceHandleNormalizesContainerExitFromRequestedStop(t *testing.T) {
	db := api.DBInstance{
		Name:          "app",
		Port:          55432,
		ContainerPort: DefaultContainerPort,
		User:          "devflow",
		Password:      "devflow",
		Image:         DefaultPostgresImage,
		ContainerName: "devflow-pg-service-stop",
		VolumeName:    "devflow-pgdata-service-stop",
	}
	engine := &runtimeServiceEngine{
		db:              db,
		watchStarted:    make(chan struct{}),
		watchExitOnStop: make(chan struct{}),
	}
	handle, err := newManagerWithDockerEngine(engine).StartRuntimeService(context.Background(), db, RuntimeServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-engine.watchStarted:
	case <-time.After(time.Second):
		t.Fatal("container watch did not start")
	}
	if err := handle.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Wait(); err != nil {
		t.Fatalf("requested container stop returned exit error: %v", err)
	}
}

func TestRuntimeServiceHandleReportsUnexpectedContainerExit(t *testing.T) {
	db := api.DBInstance{
		Name:          "app",
		Port:          55432,
		ContainerPort: DefaultContainerPort,
		User:          "devflow",
		Password:      "devflow",
		Image:         DefaultPostgresImage,
		ContainerName: "devflow-pg-service-exit",
		VolumeName:    "devflow-pgdata-service-exit",
	}
	exited := make(chan struct{})
	close(exited)
	engine := &runtimeServiceEngine{
		db:              db,
		watchStarted:    make(chan struct{}),
		watchExitOnStop: exited,
	}
	handle, err := newManagerWithDockerEngine(engine).StartRuntimeService(context.Background(), db, RuntimeServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = handle.Wait()
	var exitErr *dockerContainerExitError
	if !errors.As(err, &exitErr) || exitErr.code != 137 {
		t.Fatalf("unexpected container exit error: %v", err)
	}
}
