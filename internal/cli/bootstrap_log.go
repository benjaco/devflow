package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/benjaco/devflow/pkg/instance"
)

func tuiBootstrapLogPath(worktree string, args []string) string {
	if len(args) != 0 && args[0] != "tui" {
		return ""
	}
	id, root, err := instance.IDForWorktree(worktree)
	if err != nil {
		return ""
	}
	return instance.LogPath(root, id, "tui")
}

func writeBootstrapLog(path string, stderr io.Writer, event, fields string) {
	if path == "" {
		return
	}
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		var file *os.File
		file, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			err = file.Chmod(0o600)
			if err == nil {
				_, err = fmt.Fprintf(file, "%s level=info event=%s pid=%d ppid=%d %s\n", time.Now().UTC().Format(time.RFC3339Nano), event, os.Getpid(), os.Getppid(), fields)
			}
			if err == nil {
				// The parent may exit immediately after observing its child's status.
				err = file.Sync()
			}
			err = errors.Join(err, file.Close())
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "devflow bootstrap diagnostics: %v\n", err)
	}
}
