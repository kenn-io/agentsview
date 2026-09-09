//go:build !linux && !darwin && !windows

package sync

// stagingDirFreeBytes fails open on platforms without a capacity query;
// CreateTemp reports real failures.
func stagingDirFreeBytes(string) (uint64, bool, error) {
	return 0, false, nil
}
