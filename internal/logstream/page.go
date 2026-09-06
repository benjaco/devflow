package logstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	DefaultPageBytes = 64 << 10
	MaxPageBytes     = 1 << 20
	maxCursorBytes   = 8192
)

var (
	ErrInvalidCursor    = errors.New("invalid log cursor or cursor selection mismatch")
	ErrInvalidPageSize  = errors.New("log page size must be between 4 and 1048576 bytes")
	ErrLogResetRequired = errors.New("log was replaced or truncated; start a new read without the cursor")
	ErrInvalidUTF8      = errors.New("log contains invalid UTF-8; inspect the log file directly")
)

type LogIdentity struct {
	InstanceID string `json:"instanceId"`
	RunID      string `json:"runId"`
	Task       string `json:"task"`
	AttemptID  string `json:"attemptId"`
}

type Page struct {
	LogIdentity
	Text         string `json:"text"`
	StartOffset  int64  `json:"startOffset"`
	EndOffset    int64  `json:"endOffset"`
	NextCursor   string `json:"nextCursor"`
	AtEnd        bool   `json:"atEnd"`
	PendingBytes int    `json:"pendingBytes"`
}

type pageCursor struct {
	LogIdentity
	File   string `json:"file"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	Anchor string `json:"anchor"`
}

func decodeCursor(token string) (pageCursor, error) {
	var cursor pageCursor
	if token == "" || len(token) > maxCursorBytes {
		return cursor, ErrInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return cursor, ErrInvalidCursor
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return cursor, ErrInvalidCursor
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || cursor.File == "" || cursor.Task == "" || cursor.Offset < 0 || cursor.Size < cursor.Offset {
		return cursor, ErrInvalidCursor
	}
	anchor, err := hex.DecodeString(cursor.Anchor)
	if err != nil || len(anchor) != sha256.Size {
		return cursor, ErrInvalidCursor
	}
	return cursor, nil
}

// CursorIdentity lets callers resolve retained evidence before touching a log.
// A cursor selects an attempt; it never authorizes a filesystem path.
func CursorIdentity(token string) (LogIdentity, error) {
	cursor, err := decodeCursor(token)
	return cursor.LogIdentity, err
}

// ReadPage preserves byte offsets and partial lines without splitting UTF-8.
// An incomplete final rune waits at its original offset while an attempt runs;
// terminal evidence reports malformed text explicitly instead of polling forever.
func ReadPage(ctx context.Context, path string, identity LogIdentity, token string, limit int, terminal bool) (Page, error) {
	page := Page{LogIdentity: identity}
	if limit < utf8.UTFMax || limit > MaxPageBytes {
		return page, ErrInvalidPageSize
	}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	var cursor pageCursor
	var err error
	if token != "" {
		cursor, err = decodeCursor(token)
		if err != nil {
			return page, err
		}
		if cursor.LogIdentity != identity {
			return page, ErrInvalidCursor
		}
	}
	file, err := os.Open(path)
	if err != nil {
		if token != "" && errors.Is(err, os.ErrNotExist) {
			return page, ErrLogResetRequired
		}
		return page, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return page, err
	}
	if !info.Mode().IsRegular() {
		return page, fmt.Errorf("log %q is not a regular file", path)
	}
	identityKey, err := fileIdentity(file, info)
	if err != nil {
		return page, err
	}
	if token != "" {
		if cursor.File != identityKey || info.Size() < cursor.Size {
			return page, ErrLogResetRequired
		}
		anchor, err := pageAnchor(file, cursor.Offset)
		if err != nil {
			return page, err
		}
		if anchor != cursor.Anchor {
			return page, ErrLogResetRequired
		}
	}
	page.StartOffset = cursor.Offset
	data, err := readPageBytes(ctx, file, cursor.Offset, int(min(int64(limit), info.Size()-cursor.Offset)))
	if err != nil {
		return page, err
	}
	consumed := 0
	for consumed < len(data) {
		if !utf8.FullRune(data[consumed:]) {
			if cursor.Offset+int64(len(data)) == info.Size() {
				if terminal {
					return page, ErrInvalidUTF8
				}
				page.PendingBytes = len(data) - consumed
			}
			break
		}
		r, size := utf8.DecodeRune(data[consumed:])
		if r == utf8.RuneError && size == 1 {
			return page, ErrInvalidUTF8
		}
		consumed += size
	}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	page.EndOffset = cursor.Offset + int64(consumed)
	page.Text = string(data[:consumed])
	page.AtEnd = page.EndOffset+int64(page.PendingBytes) == info.Size()
	current, err := os.Stat(path)
	if err != nil || !os.SameFile(info, current) || current.Size() < info.Size() {
		return Page{}, errors.Join(ErrLogResetRequired, err)
	}
	if token != "" {
		anchor, err := pageAnchor(file, cursor.Offset)
		if err != nil {
			return page, err
		}
		if anchor != cursor.Anchor {
			return Page{}, ErrLogResetRequired
		}
	}
	anchor, err := pageAnchor(file, page.EndOffset)
	if err != nil {
		return page, err
	}
	encoded, err := json.Marshal(pageCursor{LogIdentity: identity, File: identityKey, Offset: page.EndOffset, Size: info.Size(), Anchor: anchor})
	if err != nil {
		return page, err
	}
	page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	if len(page.NextCursor) > maxCursorBytes {
		return Page{}, fmt.Errorf("%w: log identity exceeds cursor size limit", ErrInvalidCursor)
	}
	return page, nil
}

func readPageBytes(ctx context.Context, file io.ReaderAt, offset int64, size int) ([]byte, error) {
	data := make([]byte, size)
	if _, err := file.ReadAt(data, offset); err != nil {
		return nil, errors.Join(ErrLogResetRequired, err)
	}
	// Attempt logs append. Rechecking just this page catches an observed rewrite
	// without hashing an ever-growing log or returning a mixture of old/new bytes.
	var verify [chunkBytes]byte
	for checked := 0; checked < len(data); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		length := min(len(verify), len(data)-checked)
		if _, err := file.ReadAt(verify[:length], offset+int64(checked)); err != nil {
			return nil, errors.Join(ErrLogResetRequired, err)
		}
		if !bytes.Equal(data[checked:checked+length], verify[:length]) {
			return nil, ErrLogResetRequired
		}
		checked += length
	}
	return data, nil
}

func pageAnchor(file io.ReaderAt, offset int64) (string, error) {
	var buffer [anchorBytes]byte
	length := min(offset, int64(len(buffer)))
	if _, err := file.ReadAt(buffer[:length], offset-length); err != nil {
		return "", errors.Join(ErrLogResetRequired, err)
	}
	digest := sha256.Sum256(buffer[:length])
	return hex.EncodeToString(digest[:]), nil
}
