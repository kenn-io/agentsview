package main

import "golang.org/x/sys/windows"

// Match the first-byte lock used by flock for exclusive rotation.
func (w *lockedLogWriter) lockShared() error {
	return windows.LockFileEx(windows.Handle(w.lock.Fd()), 0, 0, 1, 0, &windows.Overlapped{})
}

func (w *lockedLogWriter) unlockShared() error {
	return windows.UnlockFileEx(windows.Handle(w.lock.Fd()), 0, 1, 0, &windows.Overlapped{})
}
