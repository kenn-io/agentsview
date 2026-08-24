package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestStoredMemberSourceUsesProviderOwnedValidation(t *testing.T) {
	root := t.TempDir()
	newProvider := func(t *testing.T, agent parser.AgentType) parser.Provider {
		provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{root}})
		require.True(t, ok)
		return provider
	}
	resolver := func(provider parser.Provider, path, id string) (string, bool) {
		return provider.(parser.StoredMemberSourceResolver).StoredMemberSource(path, id)
	}

	t.Run("Hermes aggregate and virtual members", func(t *testing.T) {
		stateDB := filepath.Join(root, "state.db")
		require.NoError(t, os.WriteFile(stateDB, nil, 0o600))
		provider, ok := parser.NewProvider(parser.AgentHermes, parser.ProviderConfig{Roots: []string{root}})
		require.True(t, ok)
		container, ok := resolver(provider, stateDB, "hermes:member-1")
		assert.True(t, ok)
		assert.Equal(t, stateDB, container)
		container, ok = resolver(provider, stateDB+"#member-1", "hermes:member-1")
		assert.True(t, ok)
		assert.Equal(t, stateDB, container)
	})

	t.Run("Hermes transcript and physical hash path", func(t *testing.T) {
		provider := newProvider(t, parser.AgentHermes)
		_, ok := resolver(provider, filepath.Join(root, "sessions", "member-1.jsonl"), "hermes:member-1")
		assert.False(t, ok)
		_, ok = resolver(provider, filepath.Join(root, "project#archive.jsonl"), "hermes:member-1")
		assert.False(t, ok)
	})

	t.Run("Devin DB member and mismatch", func(t *testing.T) {
		provider := newProvider(t, parser.AgentDevin)
		path := filepath.Join(root, "cli", "sessions.db") + "#session-1"
		container, ok := resolver(provider, path, "devin:session-1")
		assert.True(t, ok)
		assert.Equal(t, filepath.Join(root, "cli", "sessions.db"), container)
		_, ok = resolver(provider, path, "devin:session-2")
		assert.False(t, ok)
	})

	t.Run("Windsurf vscdb member and mismatch", func(t *testing.T) {
		provider := newProvider(t, parser.AgentWindsurf)
		path := filepath.Join(root, "workspaceStorage", "project", "state.vscdb") + "#session-1"
		container, ok := resolver(provider, path, "windsurf:session-1")
		assert.True(t, ok)
		assert.Equal(t, filepath.Join(root, "workspaceStorage", "project", "state.vscdb"), container)
		_, ok = resolver(provider, path, "windsurf:session-2")
		assert.False(t, ok)
	})
}

func TestConfiguredSourceMissingMembersKeepsVirtualMembersOnTombstonePolicy(t *testing.T) {
	members := []sourceMissingMember{
		{sessionID: "physical", virtual: false},
		{sessionID: "virtual", virtual: true},
	}

	got := configuredSourceMissingMembers(true, members)
	require.Len(t, got, 1)
	assert.Equal(t, "virtual", got[0].sessionID)
	assert.Equal(t, members, configuredSourceMissingMembers(false, members))
}
