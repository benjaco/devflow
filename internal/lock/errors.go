package lock

import "errors"

// ErrLocked means another open file description holds the requested lock.
var ErrLocked = errors.New("file lock is already held")
