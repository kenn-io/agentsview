package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestPollingObligationsForScopesDisableProbeForSharedDirMixedProviders(t *testing.T) {
	dir := t.TempDir()

	got := pollingObligationsForScopes(
		"watch-root",
		dir,
		[]WatchScope{
			{Agent: "opencode", SyncDir: dir},
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

func TestPollingObligationsForScopesPreserveAgentForSingleScope(t *testing.T) {
	dir := t.TempDir()

	got := pollingObligationsForScopes(
		"watch-root",
		dir,
		[]WatchScope{{Agent: string(parser.AgentOpenCode), SyncDir: dir}},
	)

	require.Len(t, got, 1)
	assert.Equal(t, PollingObligation{
		Key:   "watch-root",
		Agent: parser.AgentOpenCode,
		Roots: []string{dir},
		Probe: dir,
	}, got[0])
}
