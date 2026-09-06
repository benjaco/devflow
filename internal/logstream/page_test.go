package logstream

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPageNeverReturnsAnUnresumableCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.log")
	if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := LogIdentity{Task: strings.Repeat("task", maxCursorBytes)}
	page, err := ReadPage(context.Background(), path, identity, "", 8, true)
	if err == nil {
		if _, err := CursorIdentity(page.NextCursor); err != nil {
			t.Fatalf("reader returned a cursor that cannot resume: %v", err)
		}
	} else if !errors.Is(err, ErrInvalidCursor) {
		t.Fatal(err)
	}
}

type pageReadAtFunc func([]byte, int64) (int, error)

func (f pageReadAtFunc) ReadAt(data []byte, offset int64) (int, error) { return f(data, offset) }

func TestPageReadDetectsRewriteAndKeepsIOBounded(t *testing.T) {
	calls, total := 0, 0
	source := []byte("original evidence")
	reader := pageReadAtFunc(func(data []byte, offset int64) (int, error) {
		calls++
		total += len(data)
		if len(data) > 8 {
			t.Fatalf("unbounded read requested %d bytes", len(data))
		}
		n := copy(data, source[offset:])
		if calls == 1 {
			copy(source, "replaced")
		}
		if n < len(data) {
			return n, io.EOF
		}
		return n, nil
	})
	if _, err := readPageBytes(context.Background(), reader, 0, 8); !errors.Is(err, ErrLogResetRequired) {
		t.Fatalf("rewrite result: %v", err)
	}
	if total > 16 {
		t.Fatalf("read %d bytes for an 8-byte page", total)
	}
	calls, total = 0, 0
	stable := pageReadAtFunc(func(data []byte, _ int64) (int, error) {
		calls++
		total += len(data)
		clear(data)
		return len(data), nil
	})
	data, err := readPageBytes(context.Background(), stable, 1<<34, 8)
	if err != nil || len(data) != 8 || total != 16 {
		t.Fatalf("bounded read at large offset: bytes=%d, IO=%d, err=%v", len(data), total, err)
	}
}

func TestPagesPreserveBytesAcrossPartialLinesAndUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.log")
	content := "one\n\n€🙂six\nlast partial"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := LogIdentity{InstanceID: "instance", RunID: "run", Task: "task", AttemptID: "attempt"}
	var cursor, collected string
	for i := 0; i < len(content); i++ {
		page, err := ReadPage(context.Background(), path, identity, cursor, 4, true)
		if err != nil {
			t.Fatal(err)
		}
		if page.StartOffset != int64(len(collected)) || page.EndOffset-page.StartOffset != int64(len(page.Text)) {
			t.Fatalf("wrong byte boundary: %+v", page)
		}
		collected += page.Text
		cursor = page.NextCursor
		if page.AtEnd {
			break
		}
	}
	if collected != content {
		t.Fatalf("page bytes = %q, want %q", collected, content)
	}
	page, err := ReadPage(context.Background(), path, identity, cursor, 4, true)
	if err != nil || page.Text != "" || !page.AtEnd {
		t.Fatalf("EOF replay: %+v %v", page, err)
	}
}

func TestPageWaitsForUTF8CompletionAndRejectsInvalidTerminalBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.log")
	identity := LogIdentity{RunID: "run", Task: "task", AttemptID: "attempt"}
	if err := os.WriteFile(path, []byte{'a', 0xe2, 0x82}, 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := ReadPage(context.Background(), path, identity, "", 8, false)
	if err != nil || page.Text != "a" || page.PendingBytes != 2 || !page.AtEnd {
		t.Fatalf("partial UTF8: %+v %v", page, err)
	}
	if _, err := ReadPage(context.Background(), path, identity, page.NextCursor, 8, true); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("terminal partial UTF8: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write([]byte{0xac, '\n'})
	if err := errors.Join(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	next, err := ReadPage(context.Background(), path, identity, page.NextCursor, 8, true)
	if err != nil || next.Text != "€\n" || next.PendingBytes != 0 || !next.AtEnd {
		t.Fatalf("resumed UTF8: %+v %v", next, err)
	}
	if err := os.WriteFile(path, []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPage(context.Background(), path, identity, "", 8, false); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("invalid bytes: %v", err)
	}
}

func TestPageRejectsChangedOrMismatchedEvidence(t *testing.T) {
	identity := LogIdentity{InstanceID: "instance", RunID: "run", Task: "task", AttemptID: "attempt"}
	for _, mode := range []string{"truncate", "rewrite", "replace", "wrong_task", "wrong_instance", "wrong_attempt", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "task.log")
			if err := os.WriteFile(path, []byte("first line\nsecond\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			page, err := ReadPage(context.Background(), path, identity, "", 8, false)
			if err != nil {
				t.Fatal(err)
			}
			selected, cursor, want := identity, page.NextCursor, ErrLogResetRequired
			switch mode {
			case "truncate":
				err = os.WriteFile(path, []byte("new"), 0o600)
			case "rewrite":
				err = os.WriteFile(path, []byte("changed line\nsecond\n"), 0o600)
			case "replace":
				err = os.Rename(path, path+".old")
				if err == nil {
					err = os.WriteFile(path, []byte("first line\nsecond\n"), 0o600)
				}
			case "wrong_task":
				selected.Task = "other"
				want = ErrInvalidCursor
			case "wrong_instance":
				selected.InstanceID = "other"
				want = ErrInvalidCursor
			case "wrong_attempt":
				selected.AttemptID = "other"
				want = ErrInvalidCursor
			case "malformed":
				cursor = "not-a-cursor"
				want = ErrInvalidCursor
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReadPage(context.Background(), path, selected, cursor, 8, false); !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
		})
	}
}

func TestPageBoundsSparseLogAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	identity := LogIdentity{Task: "task"}
	page, err := ReadPage(context.Background(), path, identity, "", 32, false)
	if err != nil || len(page.Text) != 32 || page.AtEnd {
		t.Fatalf("bounded read: %+v %v", page, err)
	}
	for _, limit := range []int{0, 3, MaxPageBytes + 1} {
		if _, err := ReadPage(context.Background(), path, identity, "", limit, false); !errors.Is(err, ErrInvalidPageSize) {
			t.Fatalf("size %d: %v", limit, err)
		}
	}
	if _, err := CursorIdentity(strings.Repeat("x", 8193)); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("oversized cursor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadPage(ctx, path, identity, "", 32, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
