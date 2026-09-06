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
	Pattern  string
	Patterns []string
	Prompt   string
	Kind     PromptKind
	Repeat   bool
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
	AppendLog   bool
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

const MaxOutputLineBytes = 4 << 20

type Handle struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}
	grace time.Duration
	mu    sync.Mutex
	err   error

	stopOnce      sync.Once
	stopRequested bool
}

func NowRFC3339Nano() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func Run(ctx context.Context, spec CommandSpec) (Result, error) {
	if spec.Interactive {
		return runInteractive(ctx, spec)
	}
	cmd := CommandContext(ctx, spec.Name, spec.Args...)
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
	writer, closeWriter, err := logWriter(spec.LogPath, spec.AppendLog)
	if err != nil {
		return Result{}, err
	}
	defer closeWriter()

	if err := cmd.Start(); err != nil {
		return Result{}, err
	}

	var wg sync.WaitGroup
	var scanErrs [2]error
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanErrs[0] = scanStream(stdout, "stdout", writer, spec.OnLine)
	}()
	go func() {
		defer wg.Done()
		scanErrs[1] = scanStream(stderr, "stderr", writer, spec.OnLine)
	}()

	wg.Wait()
	waitErr := cmd.Wait()
	streamErr := errors.Join(scanErrs[0], scanErrs[1])
	err = waitErr
	if err != nil {
		if ctx.Err() != nil {
			return Result{ExitCode: -1}, ctx.Err()
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return Result{ExitCode: exitErr.ExitCode()}, errors.Join(fmt.Errorf("%s exited with code %d", spec.Name, exitErr.ExitCode()), streamErr)
		}
		return Result{}, errors.Join(err, streamErr)
	}
	if streamErr != nil {
		return Result{}, streamErr
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
	writer, closeWriter, err := logWriter(spec.LogPath, spec.AppendLog)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = closeWriter()
		return nil, err
	}

	waitCh := make(chan error, 1)
	var wg sync.WaitGroup
	var scanErrs [2]error
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanErrs[0] = scanStream(stdout, "stdout", writer, spec.OnLine)
	}()
	go func() {
		defer wg.Done()
		scanErrs[1] = scanStream(stderr, "stderr", writer, spec.OnLine)
	}()

	handle := &Handle{
		cmd:   cmd,
		done:  make(chan struct{}),
		grace: defaultGrace(spec.Grace),
	}
	go func() {
		wg.Wait()
		waitErr := normalizeStopError(handle.stopRequestedValue(), cmd.Wait())
		err := errors.Join(waitErr, scanErrs[0], scanErrs[1])
		_ = closeWriter()
		waitCh <- err
		close(waitCh)
	}()

	go func() {
		err := <-waitCh
		handle.setWaitError(err)
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

func (h *Handle) Alive() bool {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return false
	}
	select {
	case <-h.done:
		return false
	default:
		return true
	}
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
	// The process context watcher and the engine cleanup path can call Stop at
	// the same time. sync.Once blocks later callers until the first caller has
	// completed the bounded terminate/kill wait, so engine shutdown cannot
	// report completion while another goroutine is still reaping the service.
	h.stopOnce.Do(func() {
		h.mu.Lock()
		h.stopRequested = true
		h.mu.Unlock()
		if err := terminateCmd(h.cmd); err != nil {
			_ = killCmd(h.cmd)
			h.waitForStop(500 * time.Millisecond)
			return
		}
		if h.waitForStop(h.grace) {
			return
		}
		_ = killCmd(h.cmd)
		h.waitForStop(500 * time.Millisecond)
	})
	return nil
}

func (h *Handle) waitForStop(timeout time.Duration) bool {
	if timeout <= 0 {
		select {
		case <-h.done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-h.done:
		return true
	case <-timer.C:
		return false
	}
}

func (h *Handle) WriteString(value string) error {
	if h == nil || h.stdin == nil {
		return errors.New("process is not interactive")
	}
	_, err := io.WriteString(h.stdin, value)
	return err
}

func scanStream(input io.Reader, stream string, writer io.Writer, onLine func(string, string)) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), MaxOutputLineBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if onLine != nil {
			onLine(stream, line)
		}
		if writer != nil {
			_, _ = io.WriteString(writer, stream+": "+line+"\n")
		}
	}
	if err := scanner.Err(); err != nil {
		// Scanner stops consuming after an oversized token. Drain the pipe so a
		// verbose child cannot block forever while its parent waits for exit.
		_, _ = io.Copy(io.Discard, input)
		return fmt.Errorf("scan %s output: %w", stream, err)
	}
	return nil
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
	writer, closeWriter, err := logWriter(spec.LogPath, spec.AppendLog)
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

	handle := &Handle{
		cmd:   cmd,
		stdin: stdin,
		done:  make(chan struct{}),
		grace: defaultGrace(spec.Grace),
	}
	go func() {
		readWG.Wait()
		err := cmd.Wait()
		_ = stdin.Close()
		_ = closeWriter()
		waitCh <- combineInteractiveErrors(normalizeStopError(handle.stopRequestedValue(), err), reader.err())
		close(waitCh)
	}()

	go func() {
		err := <-waitCh
		handle.setWaitError(err)
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
	pattern := spec.match(r.recentBuf)
	if pattern == "" {
		return
	}
	if r.onPrompt == nil {
		r.setErr(fmt.Errorf("interactive prompt encountered without handler: %s", pattern))
		return
	}
	r.requestSeq++
	req := PromptRequest{
		ID:     fmt.Sprintf("prompt-%d", r.requestSeq),
		Prompt: firstNonEmpty(spec.Prompt, pattern),
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
	if !spec.Repeat {
		r.promptIndex++
	}
	r.recentBuf = ""
}

func (p PromptSpec) match(value string) string {
	for _, pattern := range p.patterns() {
		if pattern != "" && strings.Contains(value, pattern) {
			return pattern
		}
	}
	return ""
}

func (p PromptSpec) patterns() []string {
	if len(p.Patterns) == 0 {
		if p.Pattern == "" {
			return nil
		}
		return []string{p.Pattern}
	}
	out := make([]string, 0, len(p.Patterns)+1)
	if p.Pattern != "" {
		out = append(out, p.Pattern)
	}
	out = append(out, p.Patterns...)
	return out
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

func logWriter(path string, appendLog bool) (io.Writer, func() error, error) {
	if path == "" {
		return io.Discard, func() error { return nil }, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if appendLog {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return &lockedWriter{w: file}, file.Close, nil
}

func defaultGrace(d time.Duration) time.Duration {
	if d <= 0 {
		return 5 * time.Second
	}
	return d
}

func (h *Handle) setWaitError(err error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
	close(h.done)
}

func (h *Handle) stopRequestedValue() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopRequested
}

func normalizeStopError(stopRequested bool, err error) error {
	if !stopRequested || err == nil {
		return err
	}
	if _, ok := err.(*exec.ExitError); ok {
		return nil
	}
	return err
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}
