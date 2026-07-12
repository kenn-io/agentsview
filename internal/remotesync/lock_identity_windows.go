//go:build windows

package remotesync

import (
	"os"
	"strings"
)

func platformCanonicalLockPath(_ *os.File, path string) (string, error) {
	return strings.ToLower(path), nil
}
