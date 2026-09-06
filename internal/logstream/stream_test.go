package logstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStreamTail(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		tail          int
		want          []string
	}{
		{"empty", "", 50, nil},
		{"blank", "\n", 1, []string{""}},
		{"trailing_blank", "one\n\n", 1, []string{""}},
		{"all_blanks", "\n\n\n", 2, []string{"", ""}},
		{"terminated", "one\ntwo\nthree\n", 2, []string{"two", "three"}},
		{"unterminated", "one\ntwo\nthree", 2, []string{"two", "three"}},
		{"carriage_return_preserved", "one\r\ntwo\r\n", 2, []string{"one\r", "two\r"}},
		{"zero_means_all", "one\ntwo\nthree\n", 0, []string{"one", "two", "three"}},
		{"large_count", "one\ntwo\n", int(^uint(0) >> 1), []string{"one", "two"}},
		{"line_over_read_buffer", "skip\n" + strings.Repeat("x", chunkBytes+1) + "\nend\n", 2, []string{strings.Repeat("x", chunkBytes+1), "end"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeLog(t, tc.content)
			var lines []string
			if err := Stream(context.Background(), path, tc.tail, false, func(line string) error {
				lines = append(lines, line)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(lines, tc.want) {
				t.Fatalf("lines = %q, want %q", lines, tc.want)
			}
		})
	}
}

func TestStreamTailFollowReadsAppendsDuringInitialOutputExactlyOnce(t *testing.T) {
	// The initial snapshot spans several reads. Append while its first line is
	// delivered to cover both the reader's byte cursor and the tail/follow handoff.
	initial := "skip\nfirst\n" + strings.Repeat("x", chunkBytes+1) + "\nlast\n"
	path := writeLog(t, initial)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var lines []string
	err := Stream(ctx, path, 3, true, func(line string) error {
		lines = append(lines, line)
		if len(lines) == 1 {
			appendLog(t, path, "appended\n")
		}
		if line == "appended" {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stream error = %v, want cancellation", err)
	}
	if want := []string{"first", strings.Repeat("x", chunkBytes+1), "last", "appended"}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("read %d lines, want initial tail and append exactly once", len(lines))
	}
}

func TestStreamFollowBuffersPartialUTF8AndBlankLines(t *testing.T) {
	path := writeLog(t, "ready\nprice \xe2")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lines := make(chan string, 8)
	done := make(chan error, 1)
	go func() {
		done <- Stream(ctx, path, 0, true, func(line string) error {
			lines <- line
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("stream error = %v, want cancellation", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("stream did not stop")
		}
	})
	if line := receiveLine(t, lines); line != "ready" {
		t.Fatalf("first line = %q", line)
	}
	appendLog(t, path, "\x82")
	select {
	case line := <-lines:
		t.Fatalf("partial line emitted before newline: %q", line)
	case <-time.After(2 * pollInterval):
	}
	appendLog(t, path, "\xac\n\n")
	for _, want := range []string{"price €", ""} {
		if line := receiveLine(t, lines); line != want {
			t.Fatalf("line = %q, want %q", line, want)
		}
	}
}

func TestStreamFollowDetectsTruncationRegrowthAndReplacement(t *testing.T) {
	for _, mode := range []string{"shrink", "same_size_regrowth", "larger_regrowth", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			path := writeLog(t, "old generation\n")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lines := make(chan string, 8)
			done := make(chan error, 1)
			go func() {
				done <- Stream(ctx, path, 1, true, func(line string) error {
					lines <- line
					return nil
				})
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("stream error = %v, want cancellation", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("stream did not stop")
				}
			})
			if got := receiveLine(t, lines); got != "old generation" {
				t.Fatalf("first line = %q", got)
			}
			content := "new\n"
			switch mode {
			case "same_size_regrowth":
				content = "new generation\n"
			case "larger_regrowth":
				content = "new longer generation\n"
			case "replacement":
				// On Windows a briefly open reader can prevent deletion. Wait
				// for the poll to close its handle rather than using Unix rename
				// semantics as a test prerequisite.
				deadline := time.Now().Add(3 * time.Second)
				for {
					err := os.Remove(path)
					if err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal(err)
					}
					time.Sleep(10 * time.Millisecond)
				}
				// Force one poll while the replacement path is absent.
				time.Sleep(2 * pollInterval)
				content = "replacement generation\n"
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, want := receiveLine(t, lines), strings.TrimSuffix(content, "\n"); got != want {
				t.Fatalf("new generation = %q, want %q", got, want)
			}
		})
	}
}

func TestStreamPreservesPartialLineBeforeReplacement(t *testing.T) {
	path := writeLog(t, "old partial")
	var lines []string
	r := streamReader{ctx: context.Background(), emit: func(line string) error {
		lines = append(lines, line)
		return nil
	}}
	readPass(t, &r, path, true)
	if len(lines) != 0 {
		t.Fatalf("emitted incomplete followed line: %q", lines)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readPass(t, &r, path, false)
	if want := []string{"old partial", "new"}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
}

func TestStreamReportsRewriteDuringRead(t *testing.T) {
	initial := "first\n" + strings.Repeat("later\n", chunkBytes)
	for _, content := range []string{"short\n", initial[:chunkBytes], strings.Repeat("replacement\n", chunkBytes)} {
		t.Run(fmt.Sprintf("replacement_size_%d", len(content)), func(t *testing.T) {
			path := writeLog(t, initial)
			rewritten := false
			err := Stream(context.Background(), path, 0, false, func(string) error {
				if !rewritten {
					rewritten = true
					if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
						return err
					}
				}
				return nil
			})
			if !errors.Is(err, ErrChangedDuringRead) {
				t.Fatalf("error = %v; mixed log generations must not report successful completion", err)
			}
		})
	}
}

func TestStreamReportsErrorsAndCancellation(t *testing.T) {
	t.Run("invalid_tail", func(t *testing.T) {
		if err := Stream(context.Background(), "unused", -1, false, nil); !errors.Is(err, ErrInvalidTail) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing_file", func(t *testing.T) {
		if err := Stream(context.Background(), filepath.Join(t.TempDir(), "missing"), 1, true, nil); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("writer_error", func(t *testing.T) {
		want := errors.New("consumer stopped")
		for _, content := range []string{"line\n", "partial"} {
			err := Stream(context.Background(), writeLog(t, content), 0, false, func(string) error { return want })
			if !errors.Is(err, want) {
				t.Fatalf("writer error = %v, want %v", err, want)
			}
		}
	})
	t.Run("already_canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := Stream(ctx, "unused", 1, true, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("cancel_during_output", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		count := 0
		err := Stream(ctx, writeLog(t, "first\nsecond\n"), 0, false, func(string) error {
			count++
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) || count != 1 {
			t.Fatalf("error = %v, emitted %d lines", err, count)
		}
	})
	t.Run("idle_deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		if err := Stream(ctx, writeLog(t, ""), 0, true, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestStreamBoundsLargeFilesAndLines(t *testing.T) {
	t.Run("tail_skips_large_prefix", func(t *testing.T) {
		path := writeLog(t, "")
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		// A sparse prefix proves that a short tail need not read or retain an
		// entire large file, including its oversized lines.
		_, err = file.WriteAt([]byte("\nlast\n"), 128*1024*1024)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatal(errors.Join(err, closeErr))
		}
		var lines []string
		if err := Stream(context.Background(), path, 1, false, func(line string) error {
			lines = append(lines, line)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(lines, []string{"last"}) {
			t.Fatalf("lines = %q", lines)
		}
	})
	t.Run("all_lines_streamed", func(t *testing.T) {
		path := writeLog(t, "")
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		chunk := bytes.Repeat([]byte("line\n"), 1024)
		for range 1024 {
			if _, err := file.Write(chunk); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		count := 0
		if err := Stream(context.Background(), path, 0, false, func(line string) error {
			count++
			if line != "line" {
				return fmt.Errorf("unexpected line %q", line)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if count != 1024*1024 {
			t.Fatalf("line count = %d", count)
		}
	})
	for _, terminated := range []bool{false, true} {
		t.Run(fmt.Sprintf("maximum_line_terminated_%t", terminated), func(t *testing.T) {
			content := strings.Repeat("x", MaxLineBytes)
			if terminated {
				content += "\n"
			}
			count := 0
			err := Stream(context.Background(), writeLog(t, content), 1, false, func(line string) error {
				count++
				if len(line) != MaxLineBytes {
					t.Fatalf("line has %d bytes, want %d", len(line), MaxLineBytes)
				}
				return nil
			})
			if err != nil || count != 1 {
				t.Fatalf("error = %v, line count = %d", err, count)
			}
		})
		t.Run(fmt.Sprintf("oversized_line_terminated_%t", terminated), func(t *testing.T) {
			content := strings.Repeat("x", MaxLineBytes+1)
			if terminated {
				content += "\n"
			}
			err := Stream(context.Background(), writeLog(t, content), 1, false, func(string) error {
				t.Fatal("oversized line was silently emitted")
				return nil
			})
			if !errors.Is(err, ErrLineTooLong) {
				t.Fatalf("error = %v, want line limit error", err)
			}
		})
	}
}

func TestTailOffsetReadsOnlySuffixAndHonorsCancellation(t *testing.T) {
	reader := &countingReaderAt{ReaderAt: strings.NewReader(strings.Repeat("prefix\n", chunkBytes) + "last\n")}
	size := int64(7*chunkBytes + 5)
	offset, err := tailOffset(context.Background(), reader, size, 1)
	if err != nil || offset != size-5 || reader.bytes != chunkBytes {
		t.Fatalf("offset=%d bytes=%d error=%v", offset, reader.bytes, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader = &countingReaderAt{ReaderAt: strings.NewReader(strings.Repeat("x", 2*chunkBytes)), afterRead: cancel}
	_, err = tailOffset(ctx, reader, int64(2*chunkBytes), 1)
	if !errors.Is(err, context.Canceled) || reader.bytes != chunkBytes {
		t.Fatalf("canceled scan read %d bytes; error=%v", reader.bytes, err)
	}
}

type countingReaderAt struct {
	io.ReaderAt
	bytes     int
	afterRead func()
}

func (r *countingReaderAt) ReadAt(data []byte, offset int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(data, offset)
	r.bytes += n
	if r.afterRead != nil {
		r.afterRead()
	}
	return n, err
}

func readPass(t *testing.T, reader *streamReader, path string, first bool) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = reader.readFile(file, 0, first)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
}

func writeLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendLog(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(content)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
}

func receiveLine(t *testing.T, lines <-chan string) string {
	t.Helper()
	select {
	case line := <-lines:
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for log line")
		return ""
	}
}
