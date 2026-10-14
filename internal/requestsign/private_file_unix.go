//go:build !windows

package requestsign

import (
	"errors"
	"os"
)

func isPrivateRegularFile(file *os.File) bool {
	info, err := file.Stat()
	if err != nil || info == nil {
		return false
	}
	return info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0
}

func createPrivateFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
}

func ensurePrivateFile(file *os.File) error {
	if !isPrivateRegularFile(file) {
		return errors.New("file is not private")
	}
	return nil
}
