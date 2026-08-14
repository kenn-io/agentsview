//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !openbsd && !windows

package sync

import "errors"

func codexStagingAvailableBytes(string) (uint64, error) {
	return 0, errors.ErrUnsupported
}
