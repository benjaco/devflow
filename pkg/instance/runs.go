package instance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/benjaco/devflow/internal/jsonutil"
	"github.com/benjaco/devflow/internal/lock"
	"github.com/benjaco/devflow/pkg/api"
)

var (
	ErrInvalidRunID     = errors.New("invalid run ID")
	ErrInvalidAttemptID = errors.New("invalid attempt ID")
	ErrRunUnknown       = errors.New("unknown run")
	ErrRunExpired       = errors.New("run evidence expired")
	ErrRunFinalized     = errors.New("run is already claimed or finalized")
)

type RunRetention struct {
	MaxCompleted int
	MaxAge       time.Duration
	MaxBytes     int64
	Now          time.Time
}

type PruneResult struct {
	Removed           []string `json:"removed"`
	RetainedCompleted int      `json:"retainedCompleted"`
	RetainedBytes     int64    `json:"retainedBytes"`
}

var DefaultRunRetention = RunRetention{MaxCompleted: 100, MaxAge: 7 * 24 * time.Hour, MaxBytes: 64 << 20}

type runIndex struct {
	Issued uint64 `json:"issued"`
}

func CreateRun(worktree, instanceID string, record *api.RunRecord) error {
	if record == nil || record.RunID != "" || (record.InstanceID != "" && record.InstanceID != instanceID) {
		return fmt.Errorf("create run: %w", ErrInvalidRunID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return withRunStoreLock(ctx, worktree, instanceID, true, func() error {
		index, err := readRunIndex(worktree, instanceID)
		if err != nil {
			return err
		}
		if index.Issued == math.MaxUint64 {
			return errors.New("run identity counter exhausted")
		}
		index.Issued++
		// Retain the issued watermark even when publication fails. IDs are never
		// recycled, and absent issued IDs are expired without a tombstone per run.
		if err := jsonutil.WriteFileAtomic(runIndexPath(worktree, instanceID), index); err != nil {
			return err
		}
		next := *record
		next.RunID = fmt.Sprintf("run-%s-%016x", instanceID, index.Issued)
		next.InstanceID = instanceID
		next.State = api.RunQueued
		next.CreatedAt = time.Now().UTC()
		next.UpdatedAt = next.CreatedAt
		if next.Attempts == nil {
			next.Attempts = []api.TaskAttempt{}
		}
		path, err := RunPath(worktree, instanceID, next.RunID)
		if err != nil {
			return err
		}
		staged, err := os.MkdirTemp(runsRoot(worktree, instanceID), ".run-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(staged)
		if err := os.Mkdir(filepath.Join(staged, "attempts"), 0o700); err != nil {
			return err
		}
		if err := jsonutil.WriteFileAtomic(filepath.Join(staged, "record.json"), &next); err != nil {
			return err
		}
		// Publish a complete directory so interrupted allocation never leaves a
		// visible run whose metadata cannot be read.
		if err := os.Rename(staged, path); err != nil {
			return err
		}
		*record = next
		return nil
	})
}

func ClaimRun(worktree, instanceID, runID string, ownerPID int) (*api.RunRecord, error) {
	if _, err := RunPath(worktree, instanceID, runID); err != nil {
		return nil, err
	}
	var claimed *api.RunRecord
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := withRunStoreLock(ctx, worktree, instanceID, false, func() error {
		record, err := loadRunLocked(worktree, instanceID, runID)
		if err != nil {
			return err
		}
		if record.State != api.RunQueued {
			return ErrRunFinalized
		}
		record.State = api.RunRunning
		record.OwnerPID = ownerPID
		record.StartedAt = time.Now().UTC()
		if err := writeRunRecord(worktree, instanceID, record); err != nil {
			return err
		}
		claimed = record
		return nil
	})
	if os.IsNotExist(err) {
		err = ErrRunUnknown
	}
	return claimed, err
}

func SaveRun(worktree, instanceID string, record *api.RunRecord) error {
	if record == nil {
		return ErrInvalidRunID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return withRunStoreLock(ctx, worktree, instanceID, false, func() error {
		previous, err := loadRunLocked(worktree, instanceID, record.RunID)
		if err != nil {
			return err
		}
		if previous.State.Terminal() {
			return ErrRunFinalized
		}
		if previous.InstanceID != record.InstanceID || previous.Project != record.Project || previous.Target != record.Target || previous.Mode != record.Mode || !previous.CreatedAt.Equal(record.CreatedAt) {
			return errors.New("run identity and selection cannot change")
		}
		return writeRunRecord(worktree, instanceID, record)
	})
}

func writeRunRecord(worktree, instanceID string, record *api.RunRecord) error {
	path, err := RunPath(worktree, instanceID, record.RunID)
	if err != nil {
		return err
	}
	if record.State.Terminal() && record.FinishedAt.IsZero() {
		return errors.New("terminal run must have a completion timestamp")
	}
	switch record.State {
	case api.RunQueued, api.RunRunning, api.RunWaiting, api.RunSucceeded, api.RunFailed, api.RunCanceled:
	default:
		return fmt.Errorf("invalid run state %q", record.State)
	}
	record.UpdatedAt = time.Now().UTC()
	return jsonutil.WriteFileAtomic(filepath.Join(path, "record.json"), record)
}

func LoadRun(worktree, instanceID, runID string) (*api.RunRecord, error) {
	if _, err := RunPath(worktree, instanceID, runID); err != nil {
		return nil, err
	}
	var record *api.RunRecord
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := withRunStoreLock(ctx, worktree, instanceID, false, func() error {
		var err error
		record, err = loadRunLocked(worktree, instanceID, runID)
		return err
	})
	if os.IsNotExist(err) {
		err = ErrRunUnknown
	}
	return record, err
}

func loadRunLocked(worktree, instanceID, runID string) (*api.RunRecord, error) {
	path, err := RunPath(worktree, instanceID, runID)
	if err != nil {
		return nil, err
	}
	var record api.RunRecord
	if err := jsonutil.ReadFile(filepath.Join(path, "record.json"), &record); err == nil {
		if record.RunID != runID || record.InstanceID != instanceID {
			return nil, errors.New("persisted run identity does not match its path")
		}
		return &record, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read run %s: %w", runID, err)
	}
	index, err := readRunIndex(worktree, instanceID)
	if err != nil {
		return nil, err
	}
	sequence, _ := runSequence(runID)
	if sequence <= index.Issued {
		return nil, ErrRunExpired
	}
	return nil, ErrRunUnknown
}

func ListRuns(worktree, instanceID string) ([]api.RunRecord, error) {
	records := []api.RunRecord{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := withRunStoreLock(ctx, worktree, instanceID, false, func() error {
		var err error
		records, err = listRunsLocked(worktree, instanceID)
		return err
	})
	if os.IsNotExist(err) {
		return records, nil
	}
	return records, err
}

func listRunsLocked(worktree, instanceID string) ([]api.RunRecord, error) {
	entries, err := os.ReadDir(runsRoot(worktree, instanceID))
	if err != nil {
		return nil, err
	}
	records := []api.RunRecord{}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "run-") {
			continue
		}
		if !entry.IsDir() {
			return nil, fmt.Errorf("run evidence is not a regular directory: %s", entry.Name())
		}
		record, err := loadRunLocked(worktree, instanceID, entry.Name())
		if err != nil {
			return nil, err
		}
		records = append(records, *record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].RunID > records[j].RunID })
	return records, nil
}

// PruneRuns applies limits only to completed evidence. A long-lived watcher can
// exceed the byte budget; deleting its logs would invalidate the active record.
func PruneRuns(worktree, instanceID string, policy RunRetention) (PruneResult, error) {
	return pruneRuns(worktree, instanceID, policy, os.Rename, os.RemoveAll)
}

func pruneRuns(worktree, instanceID string, policy RunRetention, rename func(string, string) error, remove func(string) error) (PruneResult, error) {
	result := PruneResult{Removed: []string{}}
	if policy.MaxAge < 0 || policy.MaxBytes < 0 || policy.MaxCompleted < 0 {
		return result, errors.New("retention limits must not be negative")
	}
	if policy.Now.IsZero() {
		policy.Now = time.Now().UTC()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := withRunStoreLock(ctx, worktree, instanceID, false, func() error {
		if err := removeRetiredRuns(worktree, instanceID, remove); err != nil {
			return err
		}
		records, err := listRunsLocked(worktree, instanceID)
		if err != nil {
			return err
		}
		type completedRun struct {
			record api.RunRecord
			path   string
			bytes  int64
		}
		completed := []completedRun{}
		for _, record := range records {
			if !record.State.Terminal() {
				continue
			}
			path, err := RunPath(worktree, instanceID, record.RunID)
			if err != nil {
				return err
			}
			size, err := runDirectoryBytes(path)
			if err != nil {
				return err
			}
			completed = append(completed, completedRun{record: record, path: path, bytes: size})
			result.RetainedBytes += size
		}
		result.RetainedCompleted = len(completed)
		sort.Slice(completed, func(i, j int) bool {
			if completed[i].record.FinishedAt.Equal(completed[j].record.FinishedAt) {
				return completed[i].record.RunID < completed[j].record.RunID
			}
			return completed[i].record.FinishedAt.Before(completed[j].record.FinishedAt)
		})
		for _, run := range completed {
			tooOld := policy.MaxAge > 0 && policy.Now.Sub(run.record.FinishedAt) > policy.MaxAge
			tooMany := policy.MaxCompleted > 0 && result.RetainedCompleted > policy.MaxCompleted
			tooLarge := policy.MaxBytes > 0 && result.RetainedBytes > policy.MaxBytes
			if !tooOld && !tooMany && !tooLarge {
				continue
			}
			retired := filepath.Join(runsRoot(worktree, instanceID), ".prune-"+run.record.RunID)
			// Windows readers can prevent deletion of an attempt log. Retire the
			// complete directory first so partial cleanup cannot corrupt listings.
			if err := rename(run.path, retired); err != nil {
				return fmt.Errorf("retire run %s: %w", run.record.RunID, err)
			}
			result.Removed = append(result.Removed, run.record.RunID)
			result.RetainedCompleted--
			result.RetainedBytes -= run.bytes
			if err := remove(retired); err != nil {
				return fmt.Errorf("remove retired run %s: %w", run.record.RunID, err)
			}
		}
		return nil
	})
	if os.IsNotExist(err) {
		err = nil
	}
	return result, err
}

func removeRetiredRuns(worktree, instanceID string, remove func(string) error) error {
	entries, err := os.ReadDir(runsRoot(worktree, instanceID))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".prune-") {
			continue
		}
		runID := strings.TrimPrefix(entry.Name(), ".prune-")
		if _, err := RunPath(worktree, instanceID, runID); err != nil {
			return fmt.Errorf("invalid retired run directory %s: %w", entry.Name(), err)
		}
		if !entry.IsDir() {
			return fmt.Errorf("retired run is not a directory: %s", entry.Name())
		}
		// Hidden retired directories survive interrupted or blocked cleanup. The
		// issuance index already identifies them as expired; never republish them.
		if err := remove(filepath.Join(runsRoot(worktree, instanceID), entry.Name())); err != nil {
			return fmt.Errorf("remove retired run %s: %w", runID, err)
		}
	}
	return nil
}

func runDirectoryBytes(path string) (int64, error) {
	var size int64
	err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func NewAttemptID() string {
	var random [16]byte
	_, _ = rand.Read(random[:])
	return "attempt-" + hex.EncodeToString(random[:])
}

func ValidateRunID(runID string) error {
	_, err := runSequence(runID)
	return err
}

func runSequence(runID string) (uint64, error) {
	if !strings.HasPrefix(runID, "run-") {
		return 0, ErrInvalidRunID
	}
	separator := strings.LastIndexByte(runID, '-')
	if separator <= len("run-") || len(runID)-separator-1 != 16 || !validIdentityPart(runID[len("run-"):separator]) {
		return 0, ErrInvalidRunID
	}
	suffix := runID[separator+1:]
	sequence, err := strconv.ParseUint(suffix, 16, 64)
	if err != nil || sequence == 0 || fmt.Sprintf("%016x", sequence) != suffix {
		return 0, ErrInvalidRunID
	}
	return sequence, nil
}

func ValidateAttemptID(attemptID string) error {
	if !strings.HasPrefix(attemptID, "attempt-") || len(attemptID) != len("attempt-")+32 {
		return ErrInvalidAttemptID
	}
	suffix := strings.TrimPrefix(attemptID, "attempt-")
	if _, err := hex.DecodeString(suffix); err != nil || strings.ToLower(suffix) != suffix {
		return ErrInvalidAttemptID
	}
	return nil
}

func RunPath(worktree, instanceID, runID string) (string, error) {
	if err := ValidateRunID(runID); err != nil {
		return "", err
	}
	if !validIdentityPart(instanceID) || !strings.HasPrefix(runID, "run-"+instanceID+"-") || len(runID) != len("run-"+instanceID+"-")+16 {
		return "", ErrInvalidRunID
	}
	return filepath.Join(runsRoot(worktree, instanceID), runID), nil
}

func AttemptLogPath(worktree, instanceID, runID, attemptID string) (string, error) {
	path, err := RunPath(worktree, instanceID, runID)
	if err != nil {
		return "", err
	}
	if err := ValidateAttemptID(attemptID); err != nil {
		return "", err
	}
	return filepath.Join(path, "attempts", attemptID+".log"), nil
}

func validIdentityPart(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func runsRoot(worktree, instanceID string) string {
	return filepath.Join(instancePath(worktree, instanceID), "runs")
}

func runIndexPath(worktree, instanceID string) string {
	return filepath.Join(runsRoot(worktree, instanceID), "index.json")
}

func readRunIndex(worktree, instanceID string) (runIndex, error) {
	var index runIndex
	if err := jsonutil.ReadFile(runIndexPath(worktree, instanceID), &index); err != nil {
		if !os.IsNotExist(err) {
			return index, fmt.Errorf("read run identity index: %w", err)
		}
		entries, err := os.ReadDir(runsRoot(worktree, instanceID))
		if err != nil {
			return index, err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "run-") || strings.HasPrefix(entry.Name(), ".prune-") {
				return index, errors.New("run identity index missing while retained or retired evidence exists")
			}
		}
	}
	return index, nil
}

// Allocation, claims, cancellation and pruning share one short store lock so
// an observer cannot answer or cancel a run after its terminal write commits.
func withRunStoreLock(ctx context.Context, worktree, instanceID string, create bool, fn func() error) error {
	if !validIdentityPart(instanceID) {
		return ErrInvalidRunID
	}
	root := runsRoot(worktree, instanceID)
	if create {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return err
		}
	} else if _, err := os.Stat(root); err != nil {
		return err
	}
	guard, err := lock.AcquireContext(ctx, filepath.Join(root, "store.lock"))
	if err != nil {
		return err
	}
	defer guard.Release()
	if err := os.Chmod(filepath.Join(root, "store.lock"), 0o600); err != nil {
		return err
	}
	return fn()
}
