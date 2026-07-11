//go:build !darwin && !linux && !windows

package sync

import "os"

func fileChangeTime(_ string, _ os.FileInfo) (int64, bool) {
	return 0, false
}
