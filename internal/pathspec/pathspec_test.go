package pathspec

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestMatchGlobSupportsDoubleStar(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"internal/storage/**/*.sql", "internal/storage/users.sql", true},
		{"internal/storage/**/*.sql", "internal/storage/nested/users.sql", true},
		{"internal/storage/**/*.sql", "internal/storage/nested/users.go", false},
		{"**/*.prisma", "prisma/schema.prisma", true},
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},
	}
	for _, tt := range tests {
		if got := MatchGlob(tt.pattern, tt.path); got != tt.want {
			t.Fatalf("MatchGlob(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestExpandGlob(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/storage/users.sql")
	write("internal/storage/nested/rides.sql")
	write("internal/storage/nested/rides.go")

	got, err := ExpandGlob(root, "internal/storage/**/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"internal/storage/nested/rides.sql", "internal/storage/users.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExpandGlob mismatch\n got: %v\nwant: %v", got, want)
	}
}
