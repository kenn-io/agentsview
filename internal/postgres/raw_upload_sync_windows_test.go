//go:build windows

package postgres

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows"
)

func TestRawUploadDirectorySyncUnsupportedErrors(t *testing.T) {
	t.Parallel()

	for _, err := range []error{
		windows.ERROR_ACCESS_DENIED,
		windows.ERROR_INVALID_FUNCTION,
		windows.ERROR_INVALID_HANDLE,
		windows.ERROR_NOT_SUPPORTED,
	} {
		assert.True(t, isRawUploadDirectorySyncUnsupported(err))
	}
	assert.False(t, isRawUploadDirectorySyncUnsupported(errors.New("disk failure")))
}
