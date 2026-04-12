package process

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type CommandSpec struct {
	Name      string
	Args      []string
	Dir       string
	Env       map[string]string
	LogPath   string
	OnLine    func(stream, line string)
	Grace     time.Duration
	ReadyWait time.Duration
}

type Result struct {
	ExitCode int
}

type Handle struct {
	cmd   *exec.Cmd
	wait  chan error
	grace time.Duration
}

func NowRFC3339Nano() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func Run(ctx context.Context, spec CommandSpec) (Result, error) {
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	prepareCmd(cmd)
	cmd.Dir = spec.Dir
	cmd.Env = mergeEnv(spec.Env)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, err
	}
	writer, closeWriter, err := logWriter(spec.LogPath)
	if err != nil {
		return Result{}, err
	}
	defer closeWriter()

	if err := cmd.Start(); err != nil {
		return Result{}, err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go scanStream(&wg, stdout, "stdout", writer, spec.OnLine)
	go scanStream(&wg, stderr, "stderr", writer, spec.OnLine)

	err = cmd.Wait()
	wg.Wait()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return Result{ExitCode: exitErr.ExitCode()}, fmt.Errorf("%s exited with code %d", spec.Name, exitErr.ExitCode())
		}
		return Result{}, err
	}
	return Result{}, nil
}

func Start(ctx context.Context, spec CommandSpec) (*Handle, error) {
	cmd := exec.Command(spec.Name, spec.Args...)
	prepareCmd(cmd)
	cmd.Dir = spec.Dir
	cmd.Env = mergeEnv(spec.Env)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	writer, closeWriter, err := logWriter(spec.LogPath)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = closeWriter()
		return nil, err
	}

	waitCh := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go scanStream(&wg, stdout, "stdout", writer, spec.OnLine)
	go scanStream(&wg, stderr, "stderr", writer, spec.OnLine)

	go func() {
		err := cmd.Wait()
		wg.Wait()
		_ = closeWriter()
		waitCh <- err
		close(waitCh)
	}()

	go func() {
		<-ctx.Done()
		_ = stopCmd(cmd, defaultGrace(spec.Grace))
	}()

	return &Handle{
		cmd:   cmd,
		wait:  waitCh,
		grace: defaultGrace(spec.Grace),
	}, nil
}

func (h *Handle) PID() int {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

func (h *Handle) Wait() error {
	if h == nil {
		return nil
	}
	return <-h.wait
}

func (h *Handle) Stop() error {
	if h == nil || h.cmd == nil {
		return nil
	}
	return stopCmd(h.cmd, h.grace)
}

func scanStream(wg *sync.WaitGroup, input io.Reader, stream string, writer io.Writer, onLine func(string, string)) {
	defer wg.Done()
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := scanner.Text()
		if onLine != nil {
			onLine(stream, line)
		}
		if writer != nil {
			_, _ = io.WriteString(writer, stream+": "+line+"\n")
		}
	}
}

func mergeEnv(overrides map[string]string) []string {
	base := map[string]string{}
	for _, item := range os.Environ() {
		for i := 0; i < len(item); i++ {
			if item[i] == '=' {
				base[item[:i]] = item[i+1:]
				break
			}
		}
	}
	for key, value := range overrides {
		base[key] = value
	}
	env := make([]string, 0, len(base))
	for key, value := range base {
		env = append(env, key+"="+value)
	}
	return env
}

func logWriter(path string) (io.Writer, func() error, error) {
	if path == "" {
		return io.Discard, func() error { return nil }, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return file, file.Close, nil
}

func defaultGrace(d time.Duration) time.Duration {
	if d <= 0 {
		return 5 * time.Second
	}
	return d
}
