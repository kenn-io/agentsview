//go:build windows

package remotecred

import (
	"errors"
	"os"
)

func openToken(path string) (*os.File, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("token must be a regular file, not a symlink")
	}
	return os.Open(path)
}
