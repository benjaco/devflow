package validation

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/process"
)

const (
	DefaultMaxListedPaths              = 200
	DefaultMaxListedBytes        int64 = 512 * 1024
	DefaultValidationMaxFiles    int64 = 5_000_000
	DefaultValidationMaxBytes    int64 = 20 << 30
	DefaultValidationMaxTemp     int64 = 20 << 30
	DefaultValidationDiskReserve int64 = 1 << 30
)

const (
	phasePreparing  = "preparing"
	phaseProjecting = "projecting-inputs"
	phaseCopying    = "copying-files"
	phaseRunning    = "running-target"
	phaseCapturing  = "capturing-writes"
	phaseAnalyzing  = "analyzing-declarations"
	phaseArchiving  = "creating-archive"
	phaseCleaning   = "cleaning-up"
)

type resourceLimitError struct {
	failure api.ValidationResourceFailure
}

func (e *resourceLimitError) Error() string {
	if e == nil {
		return "validation resource limit exceeded"
	}
	return fmt.Sprintf("validation %s limit exceeded during %s: observed %d, limit %d", e.failure.Resource, e.failure.Phase, e.failure.Observed, e.failure.Limit)
}

type validationBudget struct {
	mu               sync.Mutex
	root             string
	maxFiles         int64
	maxBytes         int64
	maxTemporary     int64
	diskReserve      int64
	files            int64
	bytes            int64
	temporaryCurrent int64
	temporaryPeak    int64
	physicalCurrent  int64
	physicalPeak     int64
	physicalMeasured bool
	phase            string
	phases           []api.ValidationPhaseMetric
	lastDiskCheck    time.Time
}

func newValidationBudget(root string, req Request) *validationBudget {
	return &validationBudget{
		root:             root,
		maxFiles:         req.MaxFiles,
		maxBytes:         req.MaxBytes,
		maxTemporary:     req.MaxTemporaryBytes,
		diskReserve:      req.DiskSafetyReserveBytes,
		physicalMeasured: fsutil.AllocationMeasurementSupported(),
		phases:           make([]api.ValidationPhaseMetric, 0),
	}
}

func (b *validationBudget) setPhase(phase string) {
	b.mu.Lock()
	b.phase = phase
	b.mu.Unlock()
}

func (b *validationBudget) copyOptions(reporter *validationProgress) fsutil.CopyOptions {
	return fsutil.CopyOptions{
		MaxFiles: -1,
		MaxBytes: -1,
		Reserve: func(reservation fsutil.CopyReservation) error {
			if err := b.reserve(reservation.Path, reservation.Files, reservation.Bytes, reservation.Bytes); err != nil {
				return err
			}
			if reporter != nil {
				reporter.update(false)
			}
			return nil
		},
		OnProgress: func(fsutil.CopyProgress) {
			if reporter != nil {
				reporter.update(false)
			}
		},
		OnStored: func(stored fsutil.CopyStored) {
			b.recordPhysical(stored.AllocatedBytes, stored.PhysicalMeasured)
			if reporter != nil {
				reporter.update(false)
			}
		},
	}
}

func (b *validationBudget) recordPhysical(allocatedBytes int64, measured bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !measured {
		b.physicalMeasured = false
		return
	}
	b.physicalMeasured = true
	b.physicalCurrent += allocatedBytes
	if b.physicalCurrent > b.physicalPeak {
		b.physicalPeak = b.physicalCurrent
	}
}

func (b *validationBudget) process(path string, files, logicalBytes int64) error {
	return b.reserve(path, files, logicalBytes, 0)
}

func (b *validationBudget) reserve(path string, files, logicalBytes, temporaryBytes int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	nextFiles := b.files + files
	nextBytes := b.bytes + logicalBytes
	nextTemporary := b.temporaryCurrent + temporaryBytes
	if files < 0 || nextFiles < b.files {
		return b.limitErrorLocked("files_processed", nextFiles, b.maxFiles, path, 0)
	}
	if logicalBytes < 0 || nextBytes < b.bytes {
		return b.limitErrorLocked("logical_bytes_processed", nextBytes, b.maxBytes, path, 0)
	}
	if temporaryBytes < 0 || nextTemporary < b.temporaryCurrent {
		return b.limitErrorLocked("temporary_bytes", nextTemporary, b.maxTemporary, path, 0)
	}
	if b.maxFiles >= 0 && nextFiles > b.maxFiles {
		return b.limitErrorLocked("files_processed", nextFiles, b.maxFiles, path, 0)
	}
	if b.maxBytes >= 0 && nextBytes > b.maxBytes {
		return b.limitErrorLocked("logical_bytes_processed", nextBytes, b.maxBytes, path, 0)
	}
	if b.maxTemporary >= 0 && nextTemporary > b.maxTemporary {
		return b.limitErrorLocked("temporary_bytes", nextTemporary, b.maxTemporary, path, 0)
	}
	if temporaryBytes > 0 && (temporaryBytes >= 64<<20 || time.Since(b.lastDiskCheck) >= time.Second) {
		available, err := diskAvailableBytes(b.root)
		if err == nil {
			b.lastDiskCheck = time.Now()
			if available-temporaryBytes < b.diskReserve {
				return b.limitErrorLocked("available_disk", available, b.diskReserve+temporaryBytes, path, available)
			}
		}
	}
	b.files = nextFiles
	b.bytes = nextBytes
	b.temporaryCurrent = nextTemporary
	if b.temporaryCurrent > b.temporaryPeak {
		b.temporaryPeak = b.temporaryCurrent
	}
	return nil
}

func (b *validationBudget) prepareMaterialization(ctx context.Context) error {
	if err := b.refreshTemporary(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	available, err := diskAvailableBytes(b.root)
	if err != nil {
		return nil
	}
	b.lastDiskCheck = time.Now()
	if available < b.diskReserve {
		return b.limitErrorLocked("available_disk", available, b.diskReserve, b.root, available)
	}
	return nil
}

func (b *validationBudget) refreshTemporary(ctx context.Context) error {
	var (
		total            int64
		physical         int64
		physicalMeasured = fsutil.AllocationMeasurementSupported()
	)
	err := filepath.WalkDir(b.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > 0 && total > int64(^uint64(0)>>1)-info.Size() {
			return fmt.Errorf("validation temporary-byte accounting overflow")
		}
		total += info.Size()
		allocated, measured := fsutil.AllocatedFileBytes(path, info)
		if !measured {
			physicalMeasured = false
		} else if allocated > 0 && physical > int64(^uint64(0)>>1)-allocated {
			return fmt.Errorf("validation physical temporary-byte accounting overflow")
		} else {
			physical += allocated
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.temporaryCurrent = total
	if total > b.temporaryPeak {
		b.temporaryPeak = total
	}
	b.physicalCurrent = physical
	b.physicalMeasured = physicalMeasured
	if physicalMeasured && physical > b.physicalPeak {
		b.physicalPeak = physical
	}
	if b.maxTemporary >= 0 && total > b.maxTemporary {
		return b.limitErrorLocked("temporary_bytes", total, b.maxTemporary, b.root, 0)
	}
	return nil
}

func (b *validationBudget) limitErrorLocked(resource string, observed, limit int64, path string, available int64) error {
	return &resourceLimitError{failure: api.ValidationResourceFailure{
		Phase:          b.phase,
		Resource:       resource,
		Observed:       observed,
		Limit:          limit,
		AvailableBytes: available,
		ReserveBytes:   b.diskReserve,
		Path:           filepath.ToSlash(path),
	}}
}

func (b *validationBudget) snapshot() api.ValidationResourceMetrics {
	b.mu.Lock()
	defer b.mu.Unlock()
	return api.ValidationResourceMetrics{
		TotalFilesProcessed:        b.files,
		TotalLogicalBytesProcessed: b.bytes,
		TemporaryBytesCurrent:      b.temporaryCurrent,
		TemporaryBytesPeak:         b.temporaryPeak,
		TemporaryPhysicalCurrent:   b.physicalCurrent,
		TemporaryPhysicalPeak:      b.physicalPeak,
		TemporaryPhysicalMeasured:  b.physicalMeasured,
		MaxFiles:                   b.maxFiles,
		MaxLogicalBytes:            b.maxBytes,
		MaxTemporaryBytes:          b.maxTemporary,
		RemainingFiles:             remainingBudget(b.maxFiles, b.files),
		RemainingLogicalBytes:      remainingBudget(b.maxBytes, b.bytes),
		RemainingTemporaryBytes:    remainingBudget(b.maxTemporary, b.temporaryCurrent),
		DiskSafetyReserveBytes:     b.diskReserve,
		Phases:                     append([]api.ValidationPhaseMetric(nil), b.phases...),
	}
}

func remainingBudget(limit, used int64) int64 {
	if limit < 0 {
		return -1
	}
	if used >= limit {
		return 0
	}
	return limit - used
}

type validationProgress struct {
	request       Request
	budget        *validationBudget
	phase         string
	phaseStarted  time.Time
	phaseFiles    int64
	phaseBytes    int64
	issueCount    int
	lastEmittedAt time.Time
}

func newValidationProgress(req Request, budget *validationBudget) *validationProgress {
	return &validationProgress{request: req, budget: budget}
}

func (p *validationProgress) start(ctx context.Context, phase string, materialization bool) error {
	p.complete()
	p.phase = phase
	p.phaseStarted = time.Now()
	p.budget.setPhase(phase)
	metrics := p.budget.snapshot()
	p.phaseFiles = metrics.TotalFilesProcessed
	p.phaseBytes = metrics.TotalLogicalBytesProcessed
	if materialization {
		if err := p.budget.prepareMaterialization(ctx); err != nil {
			return err
		}
	}
	p.update(true)
	return nil
}

func (p *validationProgress) setIssues(count int) {
	p.issueCount = count
	p.update(false)
}

func (p *validationProgress) update(force bool) {
	if p == nil || p.phase == "" || p.request.OnEvent == nil {
		return
	}
	now := time.Now()
	if !force && now.Sub(p.lastEmittedAt) < time.Second {
		return
	}
	p.lastEmittedAt = now
	metrics := p.budget.snapshot()
	p.request.OnEvent(api.Event{
		TS:                 process.NowRFC3339Nano(),
		Type:               api.EventValidation,
		Target:             p.request.Target,
		Mode:               api.ModeValidation,
		Phase:              p.phase,
		DurationMs:         time.Since(p.phaseStarted).Milliseconds(),
		FilesProcessed:     metrics.TotalFilesProcessed,
		LogicalBytes:       metrics.TotalLogicalBytesProcessed,
		TemporaryBytes:     metrics.TemporaryBytesCurrent,
		PeakTemporaryBytes: metrics.TemporaryBytesPeak,
		RemainingBytes:     metrics.RemainingTemporaryBytes,
		IssueCount:         p.issueCount,
	})
}

func (p *validationProgress) complete() {
	if p == nil || p.phase == "" {
		return
	}
	metrics := p.budget.snapshot()
	duration := time.Since(p.phaseStarted)
	p.budget.mu.Lock()
	p.budget.phases = append(p.budget.phases, api.ValidationPhaseMetric{
		Phase:                 p.phase,
		DurationMs:            duration.Milliseconds(),
		FilesProcessed:        metrics.TotalFilesProcessed - p.phaseFiles,
		LogicalBytesProcessed: metrics.TotalLogicalBytesProcessed - p.phaseBytes,
		IssueCount:            p.issueCount,
	})
	p.budget.mu.Unlock()
	if p.request.OnEvent != nil {
		p.request.OnEvent(api.Event{
			TS:                 process.NowRFC3339Nano(),
			Type:               api.EventValidation,
			Target:             p.request.Target,
			Mode:               api.ModeValidation,
			Phase:              p.phase,
			DurationMs:         duration.Milliseconds(),
			FilesProcessed:     metrics.TotalFilesProcessed,
			LogicalBytes:       metrics.TotalLogicalBytesProcessed,
			TemporaryBytes:     metrics.TemporaryBytesCurrent,
			PeakTemporaryBytes: metrics.TemporaryBytesPeak,
			RemainingBytes:     metrics.RemainingTemporaryBytes,
			IssueCount:         p.issueCount,
			Done:               true,
		})
	}
	p.phase = ""
}
