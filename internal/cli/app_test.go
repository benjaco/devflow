package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	_ "devflow/examples/go-next-monorepo"
)

func TestGraphListJSON(t *testing.T) {
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"graph", "list", "--json", "--project", "go-next-monorepo"}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(app.Stdout.(*bytes.Buffer).Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["tasks"]; !ok {
		t.Fatalf("missing tasks: %v", payload)
	}
}
