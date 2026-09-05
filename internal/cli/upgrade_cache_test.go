package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/benjaco/devflow/pkg/instance"
)

func TestUpgradeClearsTaskCacheOnlyAfterSuccessfulInstall(t *testing.T) {
	for _, exitCode := range []int{0, 1} {
		name := "success"
		if exitCode != 0 {
			name = "install_failure"
		}
		t.Run(name, func(t *testing.T) {
			installFakeGo(t, exitCode)
			cacheRoot := instance.CacheRoot()
			entry := filepath.Join(cacheRoot, "entries", "project", "task", "old-artifact")
			if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(entry, []byte("cached"), 0o600); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(filepath.Dir(cacheRoot), "state", "keep.json")
			if err := os.MkdirAll(filepath.Dir(state), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(state, []byte("state"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			app := &App{Stdout: &stdout, Stderr: &stderr}
			err := app.Run([]string{"upgrade", "--json"})
			if (err != nil) != (exitCode != 0) {
				t.Fatalf("upgrade error=%v exitCode=%d", err, exitCode)
			}
			var result map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(entry)
			if exitCode == 0 {
				if !os.IsNotExist(statErr) {
					t.Errorf("successful upgrade kept old task cache: %v", statErr)
				}
				if result["cacheCleared"] != true {
					t.Errorf("upgrade JSON did not report cache clear: %+v", result)
				}
			} else if statErr != nil {
				t.Errorf("failed install removed task cache: %v", statErr)
			}
			if data, err := os.ReadFile(state); err != nil || string(data) != "state" {
				t.Errorf("upgrade changed unrelated state: %q %v", data, err)
			}
		})
	}
}
