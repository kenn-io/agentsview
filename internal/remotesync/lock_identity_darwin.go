//go:build darwin

package remotesync

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// platformCanonicalLockPath asks Darwin for the canonical path of the opened
// lock file. F_GETPATH reflects the volume's case and Unicode normalization
// rules without scanning any directory or globally folding path spelling.
func platformCanonicalLockPath(lockFile *os.File, _ string) (string, error) {
	buffer := make([]byte, 4096)
	if err := unix.FcntlFlock(
		lockFile.Fd(), unix.F_GETPATH,
		(*unix.Flock_t)(unsafe.Pointer(&buffer[0])),
	); err != nil {
		return "", fmt.Errorf("resolve Darwin mirror lock identity: %w", err)
	}
	before, _, ok := bytes.Cut(buffer, []byte{0})
	if !ok {
		return "", fmt.Errorf("resolve Darwin mirror lock identity: path is not terminated")
	}
	return filepath.Clean(string(before)), nil
}
