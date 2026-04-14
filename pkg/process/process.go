package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type PromptKind string

const (
	PromptConfirm PromptKind = "confirm"
	PromptText    PromptKind = "text"
)

type PromptSpec struct {
	Pattern string
	Prompt  string
	Kind    PromptKind
}

type PromptRequest struct {
	ID     string
	Prompt string
	Kind   PromptKind
}

type PromptResponse struct {
	Value string
}

type CommandSpec struct {
	Name        string
	Args        []string
	Dir         string
	Env         map[string]string
	LogPath     string
	OnLine      func(stream, line string)
	Grace       time.Duration
	ReadyWait   time.Duration
	Interactive bool
	Prompts     []PromptSpec
	OnPrompt    func(PromptRequest) (PromptResponse, error)
}

type Result struct {
	ExitCode int
}

type Handle struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}
	grace time.Duration
	mu    sync.Mutex
	err   error
}

func NowRFC3339Nano() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func Run(ctx context.Context, spec CommandSpec) (Result, error) {
	if spec.Interactive {
		return runInteractive(ctx, spec)
	}
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
		if ctx.Err() != nil {
			return Result{ExitCode: -1}, ctx.Err()
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return Result{ExitCode: exitErr.ExitCode()}, fmt.Errorf("%s exited with code %d", spec.Name, exitErr.ExitCode())
		}
		return Result{}, err
	}
	return Result{}, nil
}

func Start(ctx context.Context, spec CommandSpec) (*Handle, error) {
	if spec.Interactive {
		return startInteractive(ctx, spec)
	}
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

	handle := &Handle{
		cmd:   cmd,
		done:  make(chan struct{}),
		grace: defaultGrace(spec.Grace),
	}
	go func() {
		err := <-waitCh
		handle.mu.Lock()
		handle.err = err
		handle.mu.Unlock()
		close(handle.done)
	}()
	go func() {
		<-ctx.Done()
		_ = handle.Stop()
	}()

	return handle, nil
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
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *Handle) Stop() error {
	if h == nil || h.cmd == nil {
		return nil
	}
	if err := terminateCmd(h.cmd); err != nil {
		_ = killCmd(h.cmd)
		return nil
	}
	timer := time.NewTimer(h.grace)
	defer timer.Stop()
	select {
	case <-h.done:
		return nil
	case <-timer.C:
		_ = killCmd(h.cmd)
		select {
		case <-h.done:
		case <-time.After(500 * time.Millisecond):
		}
		return nil
	}
}

func (h *Handle) WriteString(value string) error {
	if h == nil || h.stdin == nil {
		return errors.New("process is not interactive")
	}
	_, err := io.WriteString(h.stdin, value)
	return err
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

func runInteractive(ctx context.Context, spec CommandSpec) (Result, error) {
	handle, err := startInteractive(ctx, spec)
	if err != nil {
		return Result{}, err
	}
	err = handle.Wait()
	if err != nil {
		if ctx.Err() != nil {
			return Result{ExitCode: -1}, ctx.Err()
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return Result{ExitCode: exitErr.ExitCode()}, fmt.Errorf("%s exited with code %d", spec.Name, exitErr.ExitCode())
		}
		return Result{}, err
	}
	return Result{}, nil
}

func startInteractive(ctx context.Context, spec CommandSpec) (*Handle, error) {
	cmd := exec.Command(spec.Name, spec.Args...)
	prepareCmd(cmd)
	cmd.Dir = spec.Dir
	cmd.Env = mergeEnv(spec.Env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
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
	var readWG sync.WaitGroup
	reader := &interactiveReader{
		stdin:    stdin,
		writer:   writer,
		onLine:   spec.OnLine,
		onPrompt: spec.OnPrompt,
		prompts:  spec.Prompts,
	}
	readWG.Add(2)
	go func() {
		defer readWG.Done()
		reader.read(stdout, "stdout")
	}()
	go func() {
		defer readWG.Done()
		reader.read(stderr, "stderr")
	}()

	go func() {
		err := cmd.Wait()
		readWG.Wait()
		_ = stdin.Close()
		_ = closeWriter()
		waitCh <- combineInteractiveErrors(err, reader.err())
		close(waitCh)
	}()

	handle := &Handle{
		cmd:   cmd,
		stdin: stdin,
		done:  make(chan struct{}),
		grace: defaultGrace(spec.Grace),
	}
	go func() {
		err := <-waitCh
		handle.mu.Lock()
		handle.err = err
		handle.mu.Unlock()
		close(handle.done)
	}()
	go func() {
		<-ctx.Done()
		_ = handle.Stop()
	}()

	return handle, nil
}

type interactiveReader struct {
	stdin       io.Writer
	writer      io.Writer
	onLine      func(string, string)
	onPrompt    func(PromptRequest) (PromptResponse, error)
	prompts     []PromptSpec
	promptIndex int
	requestSeq  int
	lineBuf     string
	recentBuf   string
	mu          sync.Mutex
	errMu       sync.Mutex
	readErr     error
}

func (r *interactiveReader) read(input io.Reader, stream string) {
	buf := make([]byte, 1024)
	for {
		n, err := input.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			if r.writer != nil {
				_, _ = io.WriteString(r.writer, chunk)
			}
			r.consumeChunk(stream, chunk)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				r.setErr(err)
			}
			return
		}
	}
}

func (r *interactiveReader) consumeChunk(stream, chunk string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lineBuf += chunk
	r.recentBuf += chunk
	if len(r.recentBuf) > 4096 {
		r.recentBuf = r.recentBuf[len(r.recentBuf)-4096:]
	}
	for {
		idx := indexNewline(r.lineBuf)
		if idx < 0 {
			break
		}
		line := trimLineEnding(r.lineBuf[:idx])
		if r.onLine != nil {
			r.onLine(stream, line)
		}
		r.lineBuf = r.lineBuf[idx+1:]
	}
	r.maybePrompt()
}

func (r *interactiveReader) maybePrompt() {
	if r.promptIndex >= len(r.prompts) {
		return
	}
	spec := r.prompts[r.promptIndex]
	if spec.Pattern == "" || !strings.Contains(r.recentBuf, spec.Pattern) {
		return
	}
	if r.onPrompt == nil {
		r.setErr(fmt.Errorf("interactive prompt encountered without handler: %s", spec.Pattern))
		return
	}
	r.requestSeq++
	req := PromptRequest{
		ID:     fmt.Sprintf("prompt-%d", r.requestSeq),
		Prompt: firstNonEmpty(spec.Prompt, spec.Pattern),
		Kind:   spec.Kind,
	}
	resp, err := r.onPrompt(req)
	if err != nil {
		r.setErr(err)
		return
	}
	if _, err := io.WriteString(r.stdin, resp.Value+"\n"); err != nil {
		r.setErr(err)
		return
	}
	r.promptIndex++
	r.recentBuf = ""
}

func (r *interactiveReader) setErr(err error) {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	if r.readErr == nil {
		r.readErr = err
	}
}

func (r *interactiveReader) err() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.readErr
}

func combineInteractiveErrors(waitErr, readErr error) error {
	if readErr == nil {
		return waitErr
	}
	if waitErr == nil {
		return readErr
	}
	return errors.Join(waitErr, readErr)
}

func trimLineEnding(line string) string {
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line
}

func indexNewline(value string) int {
	for i := 0; i < len(value); i++ {
		if value[i] == '\n' {
			return i
		}
	}
	return -1
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
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
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
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
