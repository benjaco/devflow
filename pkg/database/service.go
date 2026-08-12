package database

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

// RuntimeServiceOptions controls managed-container log delivery. OnLine is
// called with "stdout" or "stderr" and a single line without its newline.
type RuntimeServiceOptions struct {
	OnLine func(stream, line string)
}

// RuntimeServiceHandle adapts a Docker-managed database container to
// Devflow's generic supervised-service lifecycle without a wrapper process.
type RuntimeServiceHandle struct {
	manager *Manager
	db      api.DBInstance
	cancel  context.CancelFunc
	done    chan struct{}

	stopOnce sync.Once
	mu       sync.Mutex
	stopping bool
	err      error
	stopErr  error
}

// StartRuntimeService ensures the database container is running and starts an
// Engine API log/wait stream. The returned handle stops the container when the
// owning task context is canceled.
func (m *Manager) StartRuntimeService(ctx context.Context, db api.DBInstance, opts RuntimeServiceOptions) (*RuntimeServiceHandle, error) {
	if err := m.EnsureRuntime(ctx, db); err != nil {
		return nil, err
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	handle := &RuntimeServiceHandle{
		manager: m,
		db:      db,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go handle.watch(watchCtx, opts.OnLine)
	go func() {
		select {
		case <-ctx.Done():
			_ = handle.Stop()
		case <-handle.done:
		}
	}()
	return handle, nil
}

// PID returns zero because the supervised resource is an Engine-managed
// container rather than an operating-system child process.
func (h *RuntimeServiceHandle) PID() int { return 0 }

func (h *RuntimeServiceHandle) Alive() bool {
	if h == nil || h.done == nil {
		return false
	}
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

func (h *RuntimeServiceHandle) Wait() error {
	if h == nil || h.done == nil {
		return nil
	}
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *RuntimeServiceHandle) Stop() error {
	if h == nil || h.done == nil || h.manager == nil {
		return nil
	}
	h.stopOnce.Do(func() {
		h.mu.Lock()
		h.stopping = true
		h.mu.Unlock()

		stopCtx, cancel := context.WithTimeout(context.Background(), dockerControlTimeout+time.Second)
		h.stopErr = h.manager.StopRuntime(stopCtx, h.db)
		cancel()
		if h.cancel != nil {
			h.cancel()
		}
		select {
		case <-h.done:
		case <-time.After(dockerControlTimeout + time.Second):
		}
	})
	return h.stopErr
}

func (h *RuntimeServiceHandle) watch(ctx context.Context, onLine func(string, string)) {
	err := h.manager.watchContainer(ctx, h.db.ContainerName, onLine)
	h.mu.Lock()
	if h.stopping {
		var exitErr *dockerContainerExitError
		if errors.Is(err, context.Canceled) || errors.As(err, &exitErr) {
			err = nil
		}
	}
	h.err = err
	h.mu.Unlock()
	close(h.done)
}
