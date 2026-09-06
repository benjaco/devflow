package adaptersource

import (
	"path/filepath"
	"testing"
)

func TestIsSource(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
		want bool
	}{
		{"entrypoint", "devflow.project.go", true},
		{"companion", "devflow_tasks.go", true},
		{"empty companion stem", "devflow_.go", true},
		{"deleted companion", "devflow_deleted.go", true},
		{"renamed companion", "devflow_renamed.go", true},
		{"dot prefix", "./devflow.project.go", true},
		{"repeated dot prefix", "././devflow_tasks.go", true},
		{"native dot prefix", "." + string(filepath.Separator) + "devflow_tasks.go", true},
		{"empty", "", false},
		{"directory", ".", false},
		{"unrelated Go", "main.go", false},
		{"entrypoint test", "devflow.project_test.go", false},
		{"companion test", "devflow_tasks_test.go", false},
		{"empty stem test", "devflow_test.go", false},
		{"other extension", "devflow_tasks.go.bak", false},
		{"case differs", "Devflow_tasks.go", false},
		{"nested entrypoint", "nested/devflow.project.go", false},
		{"nested companion", "nested/devflow_tasks.go", false},
		{"prefixed nested companion", "devflow_nested/tasks.go", false},
		{"native nested companion", filepath.Join("devflow_nested", "tasks.go"), false},
		{"trailing separator", "devflow_tasks.go/", false},
		{"absolute entrypoint", "/devflow.project.go", false},
		{"absolute companion", "/devflow_tasks.go", false},
		{"windows drive", "C:/devflow_tasks.go", false},
		{"parent traversal", "../devflow_tasks.go", false},
		{"prefixed parent traversal", "./../devflow_tasks.go", false},
		{"nested traversal", "devflow_nested/../devflow_tasks.go", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSource(tt.path); got != tt.want {
				t.Fatalf("IsSource(%q) = %t, want %t", tt.path, got, tt.want)
			}
		})
	}
}
