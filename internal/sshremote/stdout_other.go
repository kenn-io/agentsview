//go:build !unix

package sshremote

import "os"

// pollableStdout returns stdout unchanged; remote serve supports Linux and
// macOS only.
func pollableStdout() (*os.File, func() error, error) {
	return os.Stdout, func() error { return nil }, nil
}
