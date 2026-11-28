//go:build !windows

package remotesync

// Other platforms report socket errors as the syscall values failure.go
// already checks.
var (
	platformConnRefusedErrnos []error
	platformUnreachableErrnos []error
)
