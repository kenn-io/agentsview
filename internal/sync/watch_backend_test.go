package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPollingObligationsForScopesDisableProbeForSharedDirMixedProviders(t *testing.T) {
	dir := t.TempDir()

	got := pollingObligationsForScopes(
		"watch-root",
		dir,
		[]WatchScope{
			{Agent: "opencode", SyncDir: dir, DegradedProbe: testDegradedProbe{}},
			{Agent: "kilo", SyncDir: dir},
		},
	)

	require.Len(t, got, 1)
	assert.Equal(t, PollingObligation{
		Key:   "watch-root",
		Roots: []string{dir},
		Probe: dir,
	}, got[0])
}
