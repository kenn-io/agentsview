package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVirtualSourcePathRoundTripAllowsHashInContainerPath(t *testing.T) {
	container := filepath.Join("/tmp", "work#1", "sessions.db")
	virtual := VirtualSourcePath(container, "session-1")

	gotContainer, gotSourceID, ok := ParseVirtualSourcePath(virtual)

	require.True(t, ok, "expected virtual source path to parse")
	assert.Equal(t, container, gotContainer)
	assert.Equal(t, "session-1", gotSourceID)
}

func TestParseVirtualSourcePathRejectsMalformedPaths(t *testing.T) {
	tests := []string{
		"",
		"/tmp/sessions.db",
		"/tmp/sessions.db#",
		"#session-1",
	}

	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			container, sourceID, ok := ParseVirtualSourcePath(path)

			assert.False(t, ok)
			assert.Empty(t, container)
			assert.Empty(t, sourceID)
		})
	}
}

func TestParseVirtualSourcePathForBase(t *testing.T) {
	path := VirtualSourcePath(filepath.Join("/tmp", "sessions.db"), "session-1")

	container, sourceID, ok := ParseVirtualSourcePathForBase(path, "sessions.db")

	require.True(t, ok, "expected base name to match")
	assert.Equal(t, filepath.Join("/tmp", "sessions.db"), container)
	assert.Equal(t, "session-1", sourceID)

	container, sourceID, ok = ParseVirtualSourcePathForBase(path, "other.db")
	assert.False(t, ok)
	assert.Empty(t, container)
	assert.Empty(t, sourceID)
}
