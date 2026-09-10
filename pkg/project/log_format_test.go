package project

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
)

func TestRuntimeRetainsReadableLogsAndOriginalEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attempt.log")
	var events []api.Event
	rt := &Runtime{
		LogPath: path, Instance: &api.Instance{ID: "instance"},
		TaskName: "check", RunID: "run", AttemptID: "attempt",
		EventFn: func(event api.Event) { events = append(events, event) },
	}
	emit := rt.LineEmitter()
	emit("stdout", "  first\n\nlast")
	emit("stderr", "  failure\n\ncontext\n")
	emit("stdout", "")
	emit("custom", "plain")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "  first\n\nlast\nE:   failure\nE: \nE: context\nE: \n\nplain\n"; got != want {
		t.Fatalf("retained log = %q, want %q", got, want)
	}
	want := []struct{ stream, line string }{
		{"stdout", "  first\n\nlast"}, {"stderr", "  failure\n\ncontext\n"}, {"stdout", ""}, {"custom", "plain"},
	}
	if len(events) != len(want) {
		t.Fatalf("event count = %d, want %d", len(events), len(want))
	}
	for i, event := range events {
		if event.Type != api.EventLogLine || event.Stream != want[i].stream || event.Line != want[i].line || event.RunID != "run" || event.AttemptID != "attempt" || event.Task != "check" {
			t.Errorf("event %d lost source data: %+v", i, event)
		}
	}
}
