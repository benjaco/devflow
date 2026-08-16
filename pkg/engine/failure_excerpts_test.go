package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
)

func TestBoundedFailureExcerptsFindEarlyGoTestFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backend_test.log")
	var log strings.Builder
	for line := 1; line <= 360; line++ {
		switch line {
		case 118:
			log.WriteString("stdout: --- FAIL: TestExample (0.01s)\n")
		case 121:
			log.WriteString("stdout: expected: 215\n")
		case 122:
			log.WriteString("stdout: actual: 229.09458\n")
		case 125:
			log.WriteString("stderr: Error: values differ\n")
		case 359:
			log.WriteString("stdout: FAIL\n")
		default:
			fmt.Fprintf(&log, "stdout: cleanup or passing package line %03d\n", line)
		}
	}
	if err := os.WriteFile(path, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if tail := boundedLogTail(path, 50, 32*1024); strings.Contains(strings.Join(tail, "\n"), "expected: 215") {
		t.Fatalf("terminal tail unexpectedly contains early assertion: %v", tail)
	}
	excerpts := boundedFailureExcerpts(path, "backend_test")
	if len(excerpts) < 1 {
		t.Fatal("expected an early failure excerpt")
	}
	first := excerpts[0]
	joined := strings.Join(first.Lines, "\n")
	if first.Reason != "go-test-failure" || first.StartLine > 118 || first.EndLine < 122 || !strings.Contains(joined, "expected: 215") || !strings.Contains(joined, "actual: 229.09458") {
		t.Fatalf("unexpected first excerpt: %+v", first)
	}
	// The Error marker is within the Go failure context and must not create a
	// duplicate window.
	for _, excerpt := range excerpts[1:] {
		if excerpt.StartLine <= 125 && excerpt.EndLine >= 118 {
			t.Fatalf("nearby markers were not merged: %+v", excerpts)
		}
	}
	assertFailureExcerptBounds(t, excerpts)
}

func TestBoundedFailureExcerptsKeepDistantFailuresIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failures.log")
	var log strings.Builder
	for line := 1; line <= 500; line++ {
		switch line {
		case 20:
			log.WriteString("panic: first failure\n")
		case 150:
			log.WriteString("src/main.go:42:7: undefined: missingName\n")
		case 300:
			log.WriteString("AssertionError: expected true\n")
		case 450:
			log.WriteString("ERROR second test group failed\n")
		default:
			fmt.Fprintf(&log, "line %d\n", line)
		}
	}
	if err := os.WriteFile(path, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	excerpts := boundedFailureExcerpts(path, "test")
	if len(excerpts) != 4 {
		t.Fatalf("distant failures = %d, want 4: %+v", len(excerpts), excerpts)
	}
	assertFailureExcerptBounds(t, excerpts)
}

func TestBoundedFailureExcerptsTruncateHugeLinesWithoutUnboundedBuffers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(strings.Repeat("x", 8*1024*1024) + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("panic: bounded reader reached this marker\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	excerpts := boundedFailureExcerpts(path, "huge")
	if len(excerpts) != 1 || !strings.Contains(strings.Join(excerpts[0].Lines, "\n"), "panic:") {
		t.Fatalf("unexpected huge-log excerpts: %+v", excerpts)
	}
	if len(excerpts[0].Lines[0]) > failureExcerptMaxLineBytes {
		t.Fatalf("oversized line was not bounded: %d", len(excerpts[0].Lines[0]))
	}
	assertFailureExcerptBounds(t, excerpts)
}

func TestBoundedFailureExcerptsReturnEmptyArrayWithoutMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.log")
	if err := os.WriteFile(path, []byte("ordinary output\ncleanup complete\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	excerpts := boundedFailureExcerpts(path, "plain")
	if excerpts == nil || len(excerpts) != 0 {
		t.Fatalf("expected a non-nil empty excerpt array, got %#v", excerpts)
	}
	data, err := json.Marshal(excerpts)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[]" {
		t.Fatalf("empty excerpts JSON = %s", data)
	}
}

func assertFailureExcerptBounds(t *testing.T, excerpts []api.FailureExcerpt) {
	t.Helper()
	if len(excerpts) > failureExcerptMaxWindows {
		t.Fatalf("excerpt windows = %d", len(excerpts))
	}
	totalLines := 0
	totalBytes := 0
	for _, excerpt := range excerpts {
		totalLines += len(excerpt.Lines)
		for _, line := range excerpt.Lines {
			totalBytes += len(line) + 1
		}
	}
	if totalLines > failureExcerptMaxLines || totalBytes > failureExcerptMaxBytes {
		t.Fatalf("excerpt bounds exceeded: lines=%d bytes=%d", totalLines, totalBytes)
	}
}
