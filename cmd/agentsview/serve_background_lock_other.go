//go:build !windows

package main

func backgroundLaunchLockErrorMeansContention(error) bool {
	return false
}
