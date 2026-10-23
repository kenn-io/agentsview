//go:build windows

package remotecred

import (
	"fmt"
	"os"
)

func openToken(path string) (*os.File, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("token must be a regular file, not a symlink")
	}
	return os.Open(path)
}
