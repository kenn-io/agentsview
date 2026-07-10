//go:build windows

package main

// Windows lock probes can surface an error for a lock held by another
// process instead of returning locked=false. Treat that uncertainty as
// contention so lifecycle commands never start a second writable daemon.
func backgroundLaunchLockErrorMeansContention(err error) bool {
	return err != nil
}
