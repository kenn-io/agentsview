package main

// TestWatchPollingObligationsKeepProvidersIndependentOnOneRoot tests that two
// agents configured at one physical root produce two independent obligations
// with distinct keys rather than one merged obligation.

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestWatchPollingObligationsKeepProvidersIndependentOnOneRoot(t *testing.T) {
	// One physical root shared by two agents.
	parent := t.TempDir()
	physicalRoot := filepath.Join(parent, "sessions")

	syncDirA := filepath.Join(parent, "agent-a-sync")
	syncDirB := filepath.Join(parent, "agent-b-sync")

	roots := []watchRoot{
		{
			path: physicalRoot,
			scopes: []watchScope{
				{agent: parser.AgentClaude, syncDir: syncDirA},
				{agent: parser.AgentOpenHands, syncDir: syncDirB},
			},
		},
	}

	// No watcher result for this root → triggers the "no watcher" branch
	// (i >= len(results)).
	got := watchPollingObligations(roots, nil, nil, nil)

	require.Len(t, got, 2,
		"two agents sharing one physical root must produce two independent obligations")

	// Each obligation must have exactly one agent's scope.
	agentSeen := make(map[parser.AgentType]bool)
	for _, ob := range got {
		require.Len(t, ob.Scopes, 1,
			"each obligation must carry exactly one agent's scope")
		agent := parser.AgentType(ob.Scopes[0].Agent)
		assert.False(t, agentSeen[agent],
			"each agent must appear in exactly one obligation")
		agentSeen[agent] = true
	}
	assert.True(t, agentSeen[parser.AgentClaude],
		"obligation for agent-a must be present")
	assert.True(t, agentSeen[parser.AgentOpenHands],
		"obligation for agent-b must be present")

	// Keys must be distinct.
	assert.NotEqual(t, got[0].Key, got[1].Key,
		"distinct agents must have distinct obligation keys")
}
