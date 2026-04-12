package project

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const DefaultReadyPollInterval = 100 * time.Millisecond

func ReadyAll(readyFns ...ReadyFunc) ReadyFunc {
	return func(ctx context.Context, rt *Runtime) error {
		for _, readyFn := range readyFns {
			if readyFn == nil {
				continue
			}
			if err := readyFn(ctx, rt); err != nil {
				return err
			}
		}
		return nil
	}
}

func ReadyFile(rel string) ReadyFunc {
	return func(ctx context.Context, rt *Runtime) error {
		path := rt.Abs(rel)
		return pollUntil(ctx, func() error {
			if _, err := os.Stat(path); err != nil {
				return err
			}
			return nil
		})
	}
}

func ReadyTCPPort(name string) ReadyFunc {
	return func(ctx context.Context, rt *Runtime) error {
		port, ok := rt.Instance.Ports[name]
		if !ok || port == 0 {
			return fmt.Errorf("named port %q not configured", name)
		}
		address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		return pollUntil(ctx, func() error {
			conn, err := net.DialTimeout("tcp", address, 150*time.Millisecond)
			if err != nil {
				return err
			}
			return conn.Close()
		})
	}
}

func ReadyHTTPNamedPort(name, path string, wantStatus int) ReadyFunc {
	return func(ctx context.Context, rt *Runtime) error {
		port, ok := rt.Instance.Ports[name]
		if !ok || port == 0 {
			return fmt.Errorf("named port %q not configured", name)
		}
		url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
		client := &http.Client{Timeout: 250 * time.Millisecond}
		return pollUntil(ctx, func() error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != wantStatus {
				return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
			}
			return nil
		})
	}
}

func ReadyPath(path string) ReadyFunc {
	return func(ctx context.Context, rt *Runtime) error {
		target := path
		if !filepath.IsAbs(target) {
			target = rt.Abs(path)
		}
		return pollUntil(ctx, func() error {
			if _, err := os.Stat(target); err != nil {
				return err
			}
			return nil
		})
	}
}

func pollUntil(ctx context.Context, probe func() error) error {
	ticker := time.NewTicker(DefaultReadyPollInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		if err := probe(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("readiness failed: %w", lastErr)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
