//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package main

import "golang.org/x/sys/unix"

func (w *lockedLogWriter) lockShared() error {
	return unix.Flock(int(w.lock.Fd()), unix.LOCK_SH)
}

func (w *lockedLogWriter) unlockShared() error {
	return unix.Flock(int(w.lock.Fd()), unix.LOCK_UN)
}
