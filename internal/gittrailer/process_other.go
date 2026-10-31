//go:build !linux && !darwin

package gittrailer

import "errors"

func processIdentity(pid uint32) (string, error) {
	return "", errors.New("process lifetime lookup is unavailable")
}
