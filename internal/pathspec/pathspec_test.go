package pathspec

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
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

func TestExpandGlobPrefixedRecursiveMatchesRootWalk(t *testing.T) {
	root := t.TempDir()
	writeTestFiles(t, root,
		"backend/app.sql",
		"backend/internal/app_query.sql",
		"backend/internal/nested/serving.sql",
		"backend/internal/nested/not-app.go",
		"frontend/app.sql",
	)
	pattern := "backend/**/*app*.sql"

	got, err := ExpandGlob(root, pattern)
	if err != nil {
		t.Fatal(err)
	}
	want := expandGlobFromRoot(t, root, pattern)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExpandGlob mismatch\n got: %v\nwant: %v", got, want)
	}
	exact := []string{"backend/app.sql", "backend/internal/app_query.sql"}
	if !reflect.DeepEqual(got, exact) {
		t.Fatalf("ExpandGlob returned unexpected matches\n got: %v\nwant: %v", got, exact)
	}
}

func TestGlobScanRootUsesLongestLiteralDirectoryPrefix(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	tests := []struct {
		name    string
		pattern string
		prefix  string
	}{
		{name: "recursive prefix", pattern: "backend/**/*app*.sql", prefix: "backend"},
		{name: "multiple literal segments", pattern: filepath.Join("backend", "internal", "**", "app*.sql"), prefix: "backend/internal"},
		{name: "filename wildcard", pattern: "backend/sqlc_*.yaml", prefix: "backend"},
		{name: "root filename wildcard", pattern: "*.go"},
		{name: "root recursive wildcard", pattern: "**/*.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pattern, err := cleanRelativeGlob(tt.pattern)
			if err != nil {
				t.Fatal(err)
			}
			got, err := globScanRoot(root, pattern)
			if err != nil {
				t.Fatal(err)
			}
			want := root
			if tt.prefix != "" {
				want = filepath.Join(root, filepath.FromSlash(tt.prefix))
			}
			if got != want {
				t.Fatalf("globScanRoot(%q) = %q, want %q", tt.pattern, got, want)
			}
		})
	}
}

func TestExpandGlobWalksOnlyLiteralPrefix(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "backend", "internal"), 0o755); err != nil {
		t.Fatal(err)
	}

	var walkedRoot string
	got, err := expandGlob(root, "backend/internal/**/*.sql", func(root string, _ fs.WalkDirFunc) error {
		walkedRoot = root
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ExpandGlob returned unexpected matches: %v", got)
	}
	want := filepath.Join(root, "backend", "internal")
	if walkedRoot != want {
		t.Fatalf("walk root = %q, want %q", walkedRoot, want)
	}
}

func TestExpandGlobFilenameWildcardUsesContainingDirectory(t *testing.T) {
	root := t.TempDir()
	writeTestFiles(t, root,
		"backend/sqlc_app.yaml",
		"backend/sqlc_frc.yaml",
		"backend/nested/sqlc_nested.yaml",
		"frontend/sqlc_app.yaml",
	)

	got, err := ExpandGlob(root, "backend/sqlc_*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"backend/sqlc_app.yaml", "backend/sqlc_frc.yaml"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExpandGlob mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestExpandGlobRootWildcardStillScansRoot(t *testing.T) {
	root := t.TempDir()
	writeTestFiles(t, root, "main.go", "other.txt", "nested/main.go")

	var walkedRoot string
	got, err := expandGlob(root, "*.go", func(root string, walkFn fs.WalkDirFunc) error {
		walkedRoot = root
		return filepath.WalkDir(root, walkFn)
	})
	if err != nil {
		t.Fatal(err)
	}
	if walkedRoot != filepath.Clean(root) {
		t.Fatalf("walk root = %q, want %q", walkedRoot, filepath.Clean(root))
	}
	want := []string{"main.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExpandGlob mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestExpandGlobRootRecursiveWildcardIncludesConventionalDirectories(t *testing.T) {
	root := t.TempDir()
	writeTestFiles(t, root,
		"backend/query.sql",
		"node_modules/dependency/query.sql",
		".git/objects/query.sql",
		".devflow/state/query.sql",
	)

	got, err := ExpandGlob(root, "**/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		".devflow/state/query.sql",
		".git/objects/query.sql",
		"backend/query.sql",
		"node_modules/dependency/query.sql",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExpandGlob mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestExpandGlobNonexistentLiteralPrefixReturnsNoMatches(t *testing.T) {
	root := t.TempDir()
	writeTestFiles(t, root, "frontend/query.sql")

	got, err := ExpandGlob(root, "backend/internal/**/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ExpandGlob returned unexpected matches: %v", got)
	}
}

func TestExpandGlobNormalizesNativeSeparators(t *testing.T) {
	root := t.TempDir()
	writeTestFiles(t, root, "backend/query.sql", "backend/nested/app.sql")
	pattern := filepath.Join("backend", "**", "*.sql")

	got, err := ExpandGlob(root, pattern)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"backend/nested/app.sql", "backend/query.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExpandGlob mismatch\n got: %v\nwant: %v", got, want)
	}
	for _, match := range got {
		if filepath.ToSlash(match) != match {
			t.Fatalf("match %q is not slash-normalized", match)
		}
	}
}

func TestExpandGlobIgnoresFilesOutsideLiteralPrefix(t *testing.T) {
	root := t.TempDir()
	writeTestFiles(t, root,
		"backend/query.sql",
		"frontend/query.sql",
		"node_modules/dependency/query.sql",
		".git/objects/query.sql",
		".devflow/state/query.sql",
	)

	got, err := ExpandGlob(root, "backend/**/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"backend/query.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExpandGlob mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestExpandGlobPropagatesRelevantSubtreeErrors(t *testing.T) {
	root := t.TempDir()
	backend := filepath.Join(root, "backend")
	if err := os.Mkdir(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("cannot read relevant subtree")

	_, err := expandGlob(root, "backend/**/*.sql", func(scanRoot string, walkFn fs.WalkDirFunc) error {
		blocked := filepath.Join(scanRoot, "blocked")
		return walkFn(blocked, nil, &fs.PathError{Op: "readdir", Path: blocked, Err: sentinel})
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ExpandGlob error = %v, want wrapped sentinel", err)
	}

	info, err := os.Lstat(backend)
	if err != nil {
		t.Fatal(err)
	}
	_, err = expandGlob(root, "backend/**/*.sql", func(scanRoot string, walkFn fs.WalkDirFunc) error {
		entry := fs.FileInfoToDirEntry(info)
		if err := walkFn(scanRoot, entry, nil); err != nil {
			return err
		}
		return walkFn(scanRoot, entry, &fs.PathError{Op: "readdir", Path: scanRoot, Err: fs.ErrNotExist})
	})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ExpandGlob post-visit root error = %v, want fs.ErrNotExist", err)
	}
}

func TestExpandGlobOrderingIsDeterministic(t *testing.T) {
	root := t.TempDir()
	writeTestFiles(t, root,
		"backend/b.sql",
		"backend/a-file.sql",
		"backend/a-dir/z.sql",
		"backend/a-dir/a.sql",
	)
	want := []string{
		"backend/a-dir/a.sql",
		"backend/a-dir/z.sql",
		"backend/a-file.sql",
		"backend/b.sql",
	}
	for i := 0; i < 3; i++ {
		got, err := ExpandGlob(root, "backend/**/*.sql")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d ordering mismatch\n got: %v\nwant: %v", i, got, want)
		}
	}
}

func TestExpandGlobDoesNotFollowDirectorySymlinksInLiteralPrefix(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	writeTestFiles(t, target, "nested/query.sql")
	if err := os.MkdirAll(filepath.Join(root, "backend"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "backend", "linked")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}

	patterns := []string{
		"backend/linked/**/*.sql",
		"backend/linked/nested/**/*.sql",
	}
	for _, pattern := range patterns {
		got, err := ExpandGlob(root, pattern)
		if err != nil {
			t.Fatalf("ExpandGlob(%q): %v", pattern, err)
		}
		if len(got) != 0 {
			t.Fatalf("ExpandGlob(%q) followed directory symlink: %v", pattern, got)
		}
	}
}

func TestExpandGlobRejectsPatternsThatCanEscapeRoot(t *testing.T) {
	root := t.TempDir()
	patterns := []string{
		"../*.go",
		"backend/../*.go",
		"/tmp/*.go",
		`C:\temp\*.go`,
		"C:/temp/*.go",
		`\\server\share\*.go`,
	}
	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			if _, err := ExpandGlob(root, pattern); err == nil {
				t.Fatalf("ExpandGlob(%q) succeeded, want path-safety error", pattern)
			}
		})
	}
}

func BenchmarkExpandGlobPrefixed(b *testing.B) {
	for _, irrelevantFiles := range []int{0, 10_000} {
		b.Run(fmt.Sprintf("irrelevant_files_%d", irrelevantFiles), func(b *testing.B) {
			root := b.TempDir()
			for i := 0; i < 32; i++ {
				writeBenchmarkFile(b, root, filepath.Join("backend", fmt.Sprintf("group-%02d", i%4), fmt.Sprintf("query-%02d.sql", i)))
			}
			for i := 0; i < irrelevantFiles; i++ {
				writeBenchmarkFile(b, root, filepath.Join("node_modules", fmt.Sprintf("package-%03d", i/100), fmt.Sprintf("file-%05d.sql", i)))
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				matches, err := ExpandGlob(root, "backend/**/*.sql")
				if err != nil {
					b.Fatal(err)
				}
				if len(matches) != 32 {
					b.Fatalf("ExpandGlob returned %d matches, want 32", len(matches))
				}
			}
		})
	}
}

func writeTestFiles(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, rel := range paths {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func expandGlobFromRoot(t *testing.T, root, pattern string) []string {
	t.Helper()
	matches := make([]string, 0)
	if err := filepath.WalkDir(root, func(full string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		rel = Clean(rel)
		if MatchGlob(pattern, rel) {
			matches = append(matches, rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return matches
}

func writeBenchmarkFile(b *testing.B, root, rel string) {
	b.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(full, nil, 0o644); err != nil {
		b.Fatal(err)
	}
}
