package process

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGracefulStopFailureAndDeadlineStillKillProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"failed", "expired"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			handle, err := Start(ctx, CommandSpec{
				Name: executable, Args: []string{"-test.run=^TestGracefulStopProcessHelper$"},
				Env: map[string]string{"DEVFLOW_GRACEFUL_STOP_HELPER": "1"}, Grace: 100 * time.Millisecond,
				GracefulStop: func(ctx context.Context) error {
					requests.Add(1)
					if mode == "expired" {
						<-ctx.Done()
						return ctx.Err()
					}
					return errors.New("shutdown protocol unavailable")
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer handle.Stop()
			cancel()
			var stopped sync.WaitGroup
			for range 8 {
				stopped.Go(func() {
					if err := handle.Stop(); err != nil {
						t.Error(err)
					}
					if handle.Alive() {
						t.Error("Stop returned before the process exited")
					}
				})
			}
			done := make(chan struct{})
			go func() { stopped.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown exceeded its grace and kill wait")
			}
			if err := handle.Wait(); err != nil {
				t.Fatal(err)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("shutdown requests = %d, want 1", got)
			}
		})
	}
}

func TestGracefulStopProcessHelper(t *testing.T) {
	if os.Getenv("DEVFLOW_GRACEFUL_STOP_HELPER") != "1" {
		return
	}
	for {
		time.Sleep(time.Second)
	}
}
