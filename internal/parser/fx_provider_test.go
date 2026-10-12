package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFxProviderDiscoveryAndCompanions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session-a", "events.jsonl")
	writeSourceFile(t, path, `{"event":{"user":{"text":"Hello"}}}`)
	for _, ignored := range []string{"events.jsonl", "session-a/other.jsonl", "session-a/subagent/events.jsonl", "invalid.id/events.jsonl", "session-a/tool-results/output.jsonl", "session-a/artifacts/events.jsonl", "session-a/logs/events.jsonl"} {
		writeSourceFile(t, filepath.Join(root, filepath.FromSlash(ignored)), `{}`)
	}
	provider, ok := NewProvider(AgentFx, ProviderConfig{Roots: []string{root}, Machine: "local"})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, path, sources[0].DisplayPath)
	located, ok, err := provider.FindSource(t.Context(), FindSourceRequest{FullSessionID: "fx:session-a"})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, path, located.DisplayPath)
	companions := []struct{ path, content string }{
		{filepath.Join(root, "session-a", "session.json"), `{"schema_version":4,"title":"Before","workspace_root":"/tmp/demo","created_at_ms":1000,"updated_at_ms":2000}`},
		{filepath.Join(root, "session-a", "usage-v2.json"), `{"snapshot":{"models":[{"model":"test","input_tokens":5,"output_tokens":2}]}}`},
		{filepath.Join(root, "session-a", "subagent", "owner.json"), `{"parent_id":"parent-a"}`},
	}
	for _, companion := range companions {
		t.Run(filepath.Base(companion.path), func(t *testing.T) {
			before, err := provider.Fingerprint(t.Context(), sources[0])
			require.NoError(t, err)
			writeSourceFile(t, companion.path, companion.content)
			after, err := provider.Fingerprint(t.Context(), sources[0])
			require.NoError(t, err)
			assert.NotEqual(t, before, after)
			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: companion.path})
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, path, changed[0].DisplayPath)
			writeSourceFile(t, companion.path, companion.content+"\n")
			updated, err := provider.Fingerprint(t.Context(), sources[0])
			require.NoError(t, err)
			assert.NotEqual(t, after, updated)
		})
	}
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	assert.Equal(t, "fx:session-a", result.Session.ID)
	assert.Equal(t, "fx:parent-a", result.Session.ParentSessionID)
	assert.Equal(t, "Before", result.Session.SessionName)
	assert.Equal(t, "Hello", result.Messages[0].Content)
	require.Len(t, result.UsageEvents, 1)
	assert.Equal(t, 5, result.UsageEvents[0].InputTokens)
	assert.Equal(t, fingerprint.Size, result.Session.File.Size)
	assert.Equal(t, fingerprint.MTimeNS, result.Session.File.Mtime)
}
