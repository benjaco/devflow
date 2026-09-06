// Package logstream reads task logs without retaining the file in memory.
package logstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const (
	// MaxLineBytes matches the process-output bound. Oversized evidence is an
	// explicit error rather than a silently truncated log line.
	MaxLineBytes = 4 * 1024 * 1024
	chunkBytes   = 32 * 1024
	anchorBytes  = 4 * 1024
	pollInterval = 250 * time.Millisecond
)

var (
	ErrInvalidTail       = errors.New("log tail must not be negative")
	ErrLineTooLong       = errors.New("log line exceeds byte limit")
	ErrChangedDuringRead = errors.New("log changed while reading; retry to retrieve the current log")
)

// Stream emits the last tail lines, then follows appends when requested. Zero
// means all lines. Follow mode waits for a newline before emitting a partial
// line; finite reads also emit the final unterminated line. Memory is bounded
// independently of the file size and requested tail count.
func Stream(ctx context.Context, path string, tail int, follow bool, emit func(string) error) error {
	if tail < 0 {
		return ErrInvalidTail
	}
	reader := streamReader{ctx: ctx, emit: emit}
	first := true
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			// A replacement may remove the path briefly. Retain the old cursor
			// until the new file exists, but do not hide an initially missing log.
			if first || !follow || !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else {
			err = reader.readFile(file, tail, first)
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				return errors.Join(err, closeErr)
			}
			first = false
		}
		if !follow {
			return reader.finishLine()
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type streamReader struct {
	ctx    context.Context
	emit   func(string) error
	info   os.FileInfo
	offset int64
	line   []byte
	anchor []byte
}

func (r *streamReader) readFile(file *os.File, tail int, first bool) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("log %q is not a regular file", file.Name())
	}
	if first {
		r.offset, err = tailOffset(r.ctx, file, info.Size(), tail)
		if err != nil {
			return err
		}
	} else {
		unchanged, err := r.cursorUnchanged(file)
		if err != nil {
			return err
		}
		if !os.SameFile(r.info, info) || info.Size() < r.offset || !unchanged {
			// Task attempts truncate their log. A short cursor anchor also
			// detects a truncate-and-regrow between polls when the size alone
			// would otherwise skip the beginning of the new attempt.
			if err := r.finishLine(); err != nil {
				return err
			}
			r.offset = 0
			r.anchor = nil
		}
	}
	r.info = info
	buffer := make([]byte, chunkBytes)
	for r.offset < info.Size() {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		unchanged, err := r.cursorUnchanged(file)
		if err != nil {
			return err
		}
		if !unchanged {
			return ErrChangedDuringRead
		}
		n, readErr := file.ReadAt(buffer[:min(int64(len(buffer)), info.Size()-r.offset)], r.offset)
		if n > 0 {
			// Advance by bytes actually consumed, never by a preceding Stat.
			// The same cursor serves initial tail output and every follow pass.
			r.offset += int64(n)
			r.rememberAnchor(buffer[:n])
			if err := r.consume(buffer[:n]); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return ErrChangedDuringRead
		}
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

func (r *streamReader) consume(data []byte) error {
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		fragment := data
		if end >= 0 {
			fragment = data[:end]
		}
		if len(r.line)+len(fragment) > MaxLineBytes {
			return fmt.Errorf("%w: maximum %d bytes", ErrLineTooLong, MaxLineBytes)
		}
		r.line = append(r.line, fragment...)
		if end < 0 {
			return nil
		}
		if err := r.ctx.Err(); err != nil {
			return err
		}
		if err := r.emit(string(r.line)); err != nil {
			return err
		}
		r.line = r.line[:0]
		data = data[end+1:]
	}
	return nil
}

func (r *streamReader) finishLine() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if len(r.line) == 0 {
		return nil
	}
	if err := r.emit(string(r.line)); err != nil {
		return err
	}
	r.line = r.line[:0]
	return nil
}

func (r *streamReader) rememberAnchor(data []byte) {
	if len(data) >= anchorBytes {
		r.anchor = append(r.anchor[:0], data[len(data)-anchorBytes:]...)
		return
	}
	keep := min(len(r.anchor), anchorBytes-len(data))
	copy(r.anchor, r.anchor[len(r.anchor)-keep:])
	r.anchor = append(r.anchor[:keep], data...)
}

func (r *streamReader) cursorUnchanged(file *os.File) (bool, error) {
	if len(r.anchor) == 0 {
		return true, nil
	}
	var buffer [anchorBytes]byte
	n, err := file.ReadAt(buffer[:len(r.anchor)], r.offset-int64(len(r.anchor)))
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return n == len(r.anchor) && bytes.Equal(buffer[:n], r.anchor), nil
}

func tailOffset(ctx context.Context, file io.ReaderAt, size int64, tail int) (int64, error) {
	if tail == 0 {
		return 0, nil
	}
	var buffer [chunkBytes]byte
	remaining := tail
	for end := size; end > 0; {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		start := max(int64(0), end-int64(len(buffer)))
		n, err := file.ReadAt(buffer[:end-start], start)
		if err != nil {
			return 0, err
		}
		for i := n - 1; i >= 0; i-- {
			// A trailing newline terminates a line; it does not create an
			// additional empty line after the end of the file.
			if buffer[i] == '\n' && start+int64(i) != size-1 {
				remaining--
				if remaining == 0 {
					return start + int64(i) + 1, nil
				}
			}
		}
		end = start
	}
	return 0, nil
}
