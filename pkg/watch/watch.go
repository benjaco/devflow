package watch

import "time"

const DefaultDebounce = 300 * time.Millisecond

// Package watch is intentionally small in the initial bootstrap.
// Incremental invalidation and live file watching are planned next.
