package process

import (
	"context"
	"errors"
	"io"
	"maps"
	"os/exec"

	"github.com/charmbracelet/x/xpty"
)

// A separate terminal satisfies child TTY detection without borrowing the TUI's
// console or input reader. The existing prompt handler remains the only answer source.
func startTerminal(ctx context.Context, spec CommandSpec) (*Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := xpty.NewPty(120, 30)
	if err != nil {
		return nil, err
	}
	output, finish, err := terminalOutput(p)
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	writer, closeWriter, err := logWriter(spec.LogPath, spec.AppendLog)
	if err != nil {
		_ = output.Close()
		_ = p.Close()
		return nil, err
	}
	cmd := exec.Command(spec.Name, spec.Args...)
	prepareCmd(cmd)
	// Describe the child's new terminal, not an outer log collector's TERM=dumb.
	env := map[string]string{"TERM": "xterm-256color"}
	maps.Copy(env, spec.Env)
	cmd.Dir, cmd.Env = spec.Dir, mergeEnv(env)
	err = ctx.Err()
	if err == nil {
		err = p.Start(cmd)
	}
	if err != nil {
		_ = output.Close()
		_ = p.Close()
		_ = closeWriter()
		return nil, err
	}
	input := terminalInput{p}
	reader := &interactiveReader{
		stdin: input, writer: writer, onLine: spec.OnLine,
		onPrompt: spec.OnPrompt, prompts: spec.Prompts, failed: make(chan struct{}),
	}
	handle := &Handle{cmd: cmd, stdin: input, done: make(chan struct{}), grace: defaultGrace(spec.Grace)}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		reader.read(output, "stdout")
	}()
	go func() {
		// Cancellation belongs to Handle.Stop so Windows still kills the tree.
		waitErr := xpty.WaitProcess(context.Background(), cmd)
		finishErr := finish()
		<-readDone
		if reader.logPartial {
			reader.writeLogChunk(reader.logStream, "\n")
		}
		closeErr := errors.Join(output.Close(), closeWriter())
		handle.setWaitError(errors.Join(
			combineInteractiveErrors(normalizeStopError(handle.stopRequestedValue(), waitErr), reader.err()),
			finishErr, closeErr,
		))
	}()
	go func() {
		select {
		case <-ctx.Done():
		case <-reader.failed:
		case <-handle.done:
			return
		}
		_ = handle.Stop()
	}()
	return handle, nil
}

type terminalInput struct{ io.Writer }

func (w terminalInput) Write(data []byte) (int, error) {
	// Terminal prompt libraries expect the Return key, including in raw mode.
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = append([]byte(nil), data...)
		data[len(data)-1] = '\r'
	}
	return w.Writer.Write(data)
}

func (terminalInput) Close() error { return nil }
