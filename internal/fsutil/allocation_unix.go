//go:build darwin || linux

package fsutil

import (
	"io/fs"
	"syscall"
)

func AllocationMeasurementSupported() bool { return true }

// AllocatedFileBytes returns filesystem blocks allocated to a regular file.
func AllocatedFileBytes(_ string, info fs.FileInfo) (int64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Blocks < 0 {
		return info.Size(), false
	}
	return stat.Blocks * 512, true
}
