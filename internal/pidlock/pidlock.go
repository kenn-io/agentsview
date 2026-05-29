// Package pidlock provides a single-instance lock backed by an OS file lock
// plus a PID marker file. The operating system releases the lock when the
// holder exits, so stale PID marker files are harmless and are overwritten by
// the next holder.
package pidlock

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Lock represents an acquired PID lock file.
type Lock struct {
	path string
	file *os.File
}

// Acquire exclusively locks path and writes the current PID into it.
// It fails if another process currently holds the lock.
func Acquire(path string) (*Lock, error) {
	file, err := openLockedFile(path)
	if err != nil {
		return nil, err
	}

	if err := writePID(file); err != nil {
		_ = releaseLockedFile(file)
		return nil, fmt.Errorf("writing lock: %w", err)
	}
	return &Lock{path: path, file: file}, nil
}

func writePID(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	_, err := file.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	return err
}

func lockHeldError(path string, cause error) error {
	data, err := os.ReadFile(path)
	if err == nil {
		if pid, ok := parsePID(data); ok {
			return fmt.Errorf("already locked by process %d (%s)", pid, path)
		}
	}
	return fmt.Errorf("already locked (%s): %w", path, cause)
}

func parsePID(data []byte) (int, bool) {
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, false
	}
	pid, err := strconv.Atoi(fields[0])
	return pid, err == nil && pid > 0
}

// Release unlocks and closes the lock file. It is safe to call more than once
// and on a nil Lock. The PID marker file is left in place so Release never
// deletes a path that may have been replaced by another process.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := releaseLockedFile(l.file)
	l.file = nil
	l.path = ""
	return err
}
