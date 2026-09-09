//go:build linux || darwin

package sync

import "golang.org/x/sys/unix"

// stagingDirFreeBytes returns the free bytes available in dir. A nil
// error with ok=false means the filesystem does not report capacity.
func stagingDirFreeBytes(dir string) (free uint64, ok bool, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false, err
	}
	return st.Bavail * uint64(st.Bsize), true, nil
}
