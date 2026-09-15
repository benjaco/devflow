package process

import (
	"io"
	"os"

	"github.com/charmbracelet/x/xpty"
	"golang.org/x/sys/windows"
)

func terminalOutput(p xpty.Pty) (io.ReadCloser, func() error, error) {
	tty := p.(*xpty.ConPty)
	var output windows.Handle
	self := windows.CurrentProcess()
	// Keep a read handle through ConPTY.Close so its final buffered output can
	// drain to EOF instead of being discarded when the library closes its pipes.
	if err := windows.DuplicateHandle(self, windows.Handle(tty.OutPipeReadFd()), self, &output, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(output), "terminal-output"), tty.Close, nil
}
