//go:build !windows

package gittrailer

import "syscall"

// executable matches Git, which runs a hook only when access(2) allows execution.
func executable(path string) bool {
	return syscall.Access(path, 0x1) == nil
}
