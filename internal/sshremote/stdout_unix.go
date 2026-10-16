//go:build unix

package sshremote

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// pollableStdout reopens fd 1 in non-blocking mode. os.NewFile then returns a
// pollable file, and closing it makes a blocked write return os.ErrClosed.
//
// The mode belongs to the open file description, which a terminal or wrapper
// process may share, so restore puts back the original mode. It works through
// a duplicate descriptor, so it still works after the pollable file closed
// fd 1. Call it only once no write is in progress.
func pollableStdout() (*os.File, func() error, error) {
	flags, err := unix.FcntlInt(1, unix.F_GETFL, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("read stdout mode: %w", err)
	}
	if flags&unix.O_NONBLOCK != 0 {
		return os.NewFile(1, "stdout"), func() error { return nil }, nil
	}
	dup, err := unix.FcntlInt(1, unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("duplicate stdout: %w", err)
	}
	if err := unix.SetNonblock(1, true); err != nil {
		return nil, nil, errors.Join(
			fmt.Errorf("set stdout non-blocking: %w", err), unix.Close(dup))
	}
	restore := func() error {
		if err := unix.SetNonblock(dup, false); err != nil {
			return errors.Join(fmt.Errorf("restore blocking stdout: %w", err), unix.Close(dup))
		}
		return unix.Close(dup)
	}
	return os.NewFile(1, "stdout"), restore, nil
}
