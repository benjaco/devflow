package engine

import (
	"context"
	"fmt"
	"sync"
)

// ServiceIdentity identifies one concrete service process. Generation remains
// useful for non-process ServiceHandle implementations whose PID is zero, and
// prevents a late Wait result from an old handle from being attributed to its
// replacement.
type ServiceIdentity struct {
	PID        int    `json:"pid,omitempty"`
	Generation uint64 `json:"generation"`
}

// ServiceLifecycleResult describes the service process actually affected by a
// daemon lifecycle request.
type ServiceLifecycleResult struct {
	Task     string          `json:"task"`
	Action   string          `json:"action"`
	Previous ServiceIdentity `json:"previous"`
	Current  ServiceIdentity `json:"current"`
	Ready    bool            `json:"ready"`
}

type serviceLifecycleCommand struct {
	action string
	task   string
	result chan serviceLifecycleResponse
}

type serviceLifecycleResponse struct {
	result ServiceLifecycleResult
	err    error
}

// LifecycleController serializes external stop and restart requests with the
// engine loop that owns the service handles. Calling handle.Stop from the
// daemon directly would make the engine interpret the expected exit as a crash
// and stop unrelated services.
type LifecycleController struct {
	commands chan serviceLifecycleCommand
	done     chan struct{}
	close    sync.Once
}

func NewLifecycleController() *LifecycleController {
	return &LifecycleController{commands: make(chan serviceLifecycleCommand), done: make(chan struct{})}
}

func (c *LifecycleController) Stop(ctx context.Context, task string) (ServiceLifecycleResult, error) {
	return c.request(ctx, "stop", task)
}

func (c *LifecycleController) Restart(ctx context.Context, task string) (ServiceLifecycleResult, error) {
	return c.request(ctx, "restart", task)
}

func (c *LifecycleController) request(ctx context.Context, action, task string) (ServiceLifecycleResult, error) {
	if c == nil {
		return ServiceLifecycleResult{}, fmt.Errorf("active run does not support service lifecycle control")
	}
	response := make(chan serviceLifecycleResponse, 1)
	command := serviceLifecycleCommand{action: action, task: task, result: response}
	select {
	case c.commands <- command:
	case <-c.done:
		return ServiceLifecycleResult{}, fmt.Errorf("active run has finished; no %s occurred", action)
	case <-ctx.Done():
		return ServiceLifecycleResult{}, ctx.Err()
	}
	select {
	case reply := <-response:
		return reply.result, reply.err
	case <-c.done:
		return ServiceLifecycleResult{}, fmt.Errorf("active run finished before %s completed", action)
	case <-ctx.Done():
		return ServiceLifecycleResult{}, ctx.Err()
	}
}

func (c *LifecycleController) closeController() {
	if c == nil {
		return
	}
	c.close.Do(func() { close(c.done) })
}
