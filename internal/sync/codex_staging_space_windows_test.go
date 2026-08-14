//go:build windows

package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexStagingAvailableBytesWindows(t *testing.T) {
	available, err := codexStagingAvailableBytes(t.TempDir())
	require.NoError(t, err)
	assert.Positive(t, available)
}
