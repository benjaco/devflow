package reporepair

import (
	"fmt"
	"strings"
	"testing"
)

func TestReportedPathsAreDeterministicAndBounded(t *testing.T) {
	paths := make([]string, 0, maxReportedPaths+50)
	for index := maxReportedPaths + 49; index >= 0; index-- {
		paths = append(paths, fmt.Sprintf("generated/%03d.sql.go", index))
	}
	reported, count, truncated := reportedPaths(paths)
	if count != maxReportedPaths+50 || len(reported) != maxReportedPaths || !truncated {
		t.Fatalf("unexpected bounded paths: count=%d listed=%d truncated=%t", count, len(reported), truncated)
	}
	if reported[0] != "generated/000.sql.go" || reported[len(reported)-1] != "generated/199.sql.go" {
		t.Fatalf("paths were not sorted deterministically: first=%q last=%q", reported[0], reported[len(reported)-1])
	}
	for _, path := range reported {
		if len(path) > maxReportedPathLen {
			t.Fatalf("reported path exceeded per-path bound: %d", len(path))
		}
	}
}

func TestParseStatusUsesNULTerminatedPaths(t *testing.T) {
	entries, err := parseStatus([]byte(" M ordinary.txt\x00?? line\nname.txt\x00A  spaced name.txt\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if entries[0].index != ' ' || entries[0].worktree != 'M' || entries[0].path != "ordinary.txt" {
		t.Fatalf("unexpected tracked entry: %+v", entries[0])
	}
	if entries[1].index != '?' || entries[1].path != "line\nname.txt" {
		t.Fatalf("newline path was not preserved: %+v", entries[1])
	}
	if entries[2].index != 'A' || entries[2].path != "spaced name.txt" {
		t.Fatalf("unexpected staged entry: %+v", entries[2])
	}
}

func TestGitErrorDetailRedactsCredentialURLsAndStaysBounded(t *testing.T) {
	detail := boundedErrorDetail("fatal: unable to access https://bot:super-secret@example.invalid/repo " + strings.Repeat("x", maxErrorDetailBytes*2))
	if strings.Contains(detail, "super-secret") || !strings.Contains(detail, "https://[REDACTED]@example.invalid/repo") {
		t.Fatalf("credential URL was not redacted: %q", detail)
	}
	if len(detail) > maxErrorDetailBytes {
		t.Fatalf("error detail exceeded bound: %d", len(detail))
	}
}

func TestGitPathChunksAreDeterministicBoundedAndLossless(t *testing.T) {
	paths := []string{"z-last.txt", "duplicate.txt", "duplicate.txt"}
	for index := 0; index < 200; index++ {
		paths = append(paths, fmt.Sprintf("generated/%03d-%s.sql.go", index, strings.Repeat("x", 100)))
	}
	chunks := gitPathChunks(paths)
	if len(chunks) < 2 {
		t.Fatalf("expected argument chunking, got %d chunk(s)", len(chunks))
	}
	flattened := make([]string, 0)
	for _, chunk := range chunks {
		chunkBytes := 0
		for _, path := range chunk {
			chunkBytes += len(path) + 1
		}
		if chunkBytes > maxGitPathArgBytes && len(chunk) > 1 {
			t.Fatalf("multi-path chunk exceeded bound: paths=%d bytes=%d", len(chunk), chunkBytes)
		}
		flattened = append(flattened, chunk...)
	}
	want := uniqueSorted(paths)
	if !samePaths(flattened, want) || strings.Join(flattened, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("chunked paths changed order or membership")
	}
}
