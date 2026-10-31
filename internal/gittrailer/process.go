package gittrailer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
)

// The shell supplies its Git parent's PID. Its lifetime binds the two hooks
// without trusting stale state or adding markers to the editor's message.
// This never discovers an agent: session IDs come exclusively from Discover.
func invocationForPID(value string) (string, error) {
	pid, err := strconv.ParseUint(value, 10, 32)
	if err != nil || pid == 0 {
		return "", errors.New("cannot identify the invoking Git process")
	}
	identity, err := processIdentity(uint32(pid))
	if err != nil {
		return "", errors.New("cannot identify the invoking Git process")
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:]), nil
}
