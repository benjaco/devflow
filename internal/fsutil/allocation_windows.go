//go:build windows

package fsutil

import "io/fs"

func AllocationMeasurementSupported() bool { return false }

// AllocatedFileBytes falls back to logical size where portable allocation
// metadata is unavailable through os.FileInfo.
func AllocatedFileBytes(_ string, info fs.FileInfo) (int64, bool) {
	return info.Size(), false
}
