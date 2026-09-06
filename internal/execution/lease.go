// Package execution reserves a worktree for one executor before it mutates
// runtime state or resources. A retained ownership record requires explicit
// reconciliation even after the OS has released a crashed process's lock.
package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/internal/jsonutil"
	"github.com/benjaco/devflow/internal/lock"
)

// Owner identifies the execution responsible for a worktree. Acquire supplies
// Token, PID, Worktree and AcquiredAt; callers describe Target, Mode and Kind.
// This record deliberately contains no runtime environment or credentials.
type Owner struct {
	Token      string    `json:"token"`
	PID        int       `json:"pid"`
	Worktree   string    `json:"worktree"`
	Target     string    `json:"target,omitempty"`
	Mode       string    `json:"mode,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	AcquiredAt time.Time `json:"acquiredAt"`
}

// ConflictError reports an active owner or abandoned execution awaiting
// reconciliation. Owner can be nil during an owner's metadata publication.
type ConflictError struct {
	Worktree         string
	Owner            *Owner
	RecoveryRequired bool
	Cause            error
}

func (*ConflictError) Code() string { return "resource_conflict" }

func (e *ConflictError) Error() string {
	message := "worktree execution is already owned"
	if e.RecoveryRequired {
		message = "worktree execution requires recovery before another run can start"
	}
	if e.Owner != nil {
		message += fmt.Sprintf(" (pid %d, target %q, mode %q)", e.Owner.PID, e.Owner.Target, e.Owner.Mode)
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *ConflictError) Unwrap() error { return e.Cause }

type options struct {
	recover func(Owner) error
}

type Option func(*options)

// WithRecovery permits replacing a retained owner only after reconcile returns
// nil. The callback runs while holding the worktree lock and must establish that
// resources left by the previous execution no longer conflict. A dead PID alone
// cannot establish this: children and external resources can outlive it.
func WithRecovery(reconcile func(Owner) error) Option {
	return func(opts *options) { opts.recover = reconcile }
}

type Lease struct {
	mu               sync.Mutex
	fileLock         *lock.FileLock
	owner            Owner
	recoveryRequired bool
}

// Acquire immediately rejects a competing execution, including one in the same
// process. It never modifies the previous owner's marker on rejection.
func Acquire(worktree string, owner Owner, opts ...Option) (*Lease, error) {
	real, err := canonicalWorktree(worktree)
	if err != nil {
		return nil, fmt.Errorf("resolve execution worktree: %w", err)
	}
	fileLock, err := lock.TryAcquire(filepath.Join(real, ".devflow", "execution.lock"))
	if err != nil {
		if errors.Is(err, lock.ErrLocked) {
			previous, readErr := readOwner(real)
			return nil, &ConflictError{Worktree: real, Owner: previous, Cause: readErr}
		}
		return nil, fmt.Errorf("acquire worktree execution: %w", err)
	}
	accepted := false
	defer func() {
		if !accepted {
			_ = fileLock.Release()
		}
	}()
	previous, err := readOwner(real)
	if err != nil {
		return nil, &ConflictError{Worktree: real, RecoveryRequired: true, Cause: err}
	}
	var settings options
	for _, opt := range opts {
		opt(&settings)
	}
	if previous != nil {
		if settings.recover == nil {
			return nil, &ConflictError{Worktree: real, Owner: previous, RecoveryRequired: true}
		}
		if err := settings.recover(*previous); err != nil {
			return nil, &ConflictError{Worktree: real, Owner: previous, RecoveryRequired: true, Cause: err}
		}
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, fmt.Errorf("create execution ownership token: %w", err)
	}
	owner.Token = hex.EncodeToString(token[:])
	owner.PID = os.Getpid()
	owner.Worktree = real
	owner.AcquiredAt = time.Now().UTC()
	if err := jsonutil.WriteFileAtomic(ownerPath(real), owner); err != nil {
		return nil, fmt.Errorf("persist worktree execution owner: %w", err)
	}
	accepted = true
	return &Lease{fileLock: fileLock, owner: owner}, nil
}

// ReadOwner reads the diagnostic/recovery record without creating state. The
// presence of a record does not distinguish an active owner from an abandoned
// execution; only acquiring the OS lock can establish that distinction.
func ReadOwner(worktree string) (*Owner, error) {
	real, err := canonicalWorktree(worktree)
	if err != nil {
		return nil, err
	}
	return readOwner(real)
}

func readOwner(worktree string) (*Owner, error) {
	var owner Owner
	if err := jsonutil.ReadFile(ownerPath(worktree), &owner); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read worktree execution owner: %w", err)
	}
	if owner.Token == "" || owner.PID <= 0 || owner.Worktree == "" || owner.AcquiredAt.IsZero() {
		return nil, errors.New("worktree execution owner record is incomplete")
	}
	return &owner, nil
}

func ownerPath(worktree string) string {
	return filepath.Join(worktree, ".devflow", "execution-owner.json")
}

func canonicalWorktree(worktree string) (string, error) {
	abs, err := filepath.Abs(worktree)
	if err != nil {
		return "", err
	}
	return fsutil.Realpath(abs)
}

func (l *Lease) Owner() Owner {
	if l == nil {
		return Owner{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.owner
}

// ValidFor permits an engine to reuse its caller's live lease. An absent,
// released, abandoned, recovery-required or different-worktree lease cannot
// grant admission.
func (l *Lease) ValidFor(worktree string) bool {
	if l == nil {
		return false
	}
	real, err := canonicalWorktree(worktree)
	if err != nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fileLock != nil && !l.recoveryRequired && l.owner.Worktree == real
}

// Release clears the ownership record and unlocks the worktree. The caller must
// have confirmed resource cleanup first, or called RequireRecovery to preserve
// the record. A Release after Abandon leaves its recovery marker intact.
func (l *Lease) Release() error { return l.close(true) }

// RequireRecovery marks cleanup incomplete without unlocking the worktree.
// Call this before writing final status or restoring an enclosing operation's
// temporary environment. It immediately prevents borrowing this lease for more
// execution; Release retains the recovery marker once finalization is complete.
func (l *Lease) RequireRecovery() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recoveryRequired = true
}

// Abandon unlocks the worktree while preserving the recovery record. It never
// grants another execution permission to proceed without reconciliation. Use
// RequireRecovery while the current owner still has finalization work to do.
func (l *Lease) Abandon() error { return l.close(false) }

func (l *Lease) close(clean bool) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fileLock == nil {
		return nil
	}
	var markerErr error
	if clean && !l.recoveryRequired {
		previous, err := readOwner(l.owner.Worktree)
		if err != nil {
			markerErr = err
		} else if previous != nil && previous.Token != l.owner.Token {
			markerErr = errors.New("worktree execution ownership changed before release")
		} else if err := os.Remove(ownerPath(l.owner.Worktree)); err != nil && !os.IsNotExist(err) {
			markerErr = fmt.Errorf("remove worktree execution owner: %w", err)
		}
	}
	unlockErr := l.fileLock.Release()
	l.fileLock = nil
	return errors.Join(markerErr, unlockErr)
}

type contextKey struct{}

func ContextWithLease(ctx context.Context, lease *Lease) context.Context {
	return context.WithValue(ctx, contextKey{}, lease)
}

func FromContext(ctx context.Context) *Lease {
	lease, _ := ctx.Value(contextKey{}).(*Lease)
	return lease
}
