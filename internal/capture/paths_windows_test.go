//go:build windows

package capture

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCapturePreflightAcceptsPathsOnDifferentWindowsVolumes(t *testing.T) {
	require.NoError(t, validateProviderRootPath(`C:\capture`, `D:\provider`))

	result, err := validateResultPath(`C:\capture`, `D:\usage.json`)
	require.NoError(t, err)
	assert.Equal(t, `D:\usage.json`, result)
}
