//go:build windows

package artifact

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows"
)

func TestWindowsDirectorySyncAccessDeniedIsUnsupported(t *testing.T) {
	t.Parallel()

	assert.True(t, isFolderDirectorySyncUnsupported(&os.PathError{
		Op:   "sync",
		Path: "artifact-folder",
		Err:  windows.ERROR_ACCESS_DENIED,
	}))
	assert.False(t, isFolderDirectorySyncUnsupported(&os.PathError{
		Op:   "sync",
		Path: "artifact-folder",
		Err:  windows.ERROR_DISK_FULL,
	}))
}
