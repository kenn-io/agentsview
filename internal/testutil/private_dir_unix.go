//go:build !windows

package testutil

import "testing"

// PrivateTempDir creates a temporary directory whose files are private to the user.
func PrivateTempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
