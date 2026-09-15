//go:build !windows

package process

import (
	"errors"
	"io"
	"syscall"

	"github.com/charmbracelet/x/xpty"
)

func terminalOutput(p xpty.Pty) (io.ReadCloser, func() error, error) {
	tty := p.(*xpty.UnixPty)
	// The parent's slave descriptor otherwise keeps the master from reaching EOF.
	return terminalReader{tty.Master()}, tty.Slave().Close, nil
}

type terminalReader struct{ io.ReadCloser }

func (r terminalReader) Read(data []byte) (int, error) {
	n, err := r.ReadCloser.Read(data)
	if errors.Is(err, syscall.EIO) {
		err = io.EOF // Unix PTYs report a closed slave as EIO.
	}
	return n, err
}
