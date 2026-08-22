//go:build windows

package capture

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func assertPrivateMode(t *testing.T, path string, _ os.FileMode) {
	t.Helper()
	require.NoError(t, verifyCapturePathOwner(path))
}
