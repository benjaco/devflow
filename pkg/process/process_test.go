package process

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestRunCapturesStdoutAndStderr(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "task.log")
	lines := map[string][]string{}
	var mu sync.Mutex
	_, err := Run(context.Background(), CommandSpec{
		Name:    "sh",
		Args:    []string{"-c", "printf 'out\\n'; printf 'err\\n' >&2"},
		LogPath: logPath,
		OnLine: func(stream, line string) {
			mu.Lock()
			defer mu.Unlock()
			lines[stream] = append(lines[stream], line)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines["stdout"]) != 1 || lines["stdout"][0] != "out" {
		t.Fatalf("stdout lines = %v", lines["stdout"])
	}
	if len(lines["stderr"]) != 1 || lines["stderr"][0] != "err" {
		t.Fatalf("stderr lines = %v", lines["stderr"])
	}
}
