//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package postgres

import (
	"errors"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRawUploadDirectorySyncUnsupportedErrors(t *testing.T) {
	t.Parallel()

	for _, err := range []error{syscall.EINVAL, syscall.ENOSYS, syscall.ENOTSUP} {
		assert.True(t, isRawUploadDirectorySyncUnsupported(err))
	}
	assert.False(t, isRawUploadDirectorySyncUnsupported(errors.New("disk failure")))
}
