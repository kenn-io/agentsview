//go:build !windows && !darwin

package remotesync

import "os"

func platformCanonicalLockPath(_ *os.File, path string) (string, error) {
	return path, nil
}
