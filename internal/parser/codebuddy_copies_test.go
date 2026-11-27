package parser

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A lookup by session id checks every copy of a converted conversation. An
// empty or metadata-only copy in the first home, even a newer one, does not
// hide a copy with turns in another home, and the frozen 4.x history yields
// to that copy.
func TestCodeBuddyLookupPrefersCopyWithTurns(t *testing.T) {
	metaOnly := `{"type":"session-meta","id":"m","sessionId":"conv1"}`
	turn := `{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"content":[{"type":"input_text","text":"continued in 5.x"}]}`
	for _, tc := range []struct {
		name  string
		first []string
	}{
		{"metadata-only copy", []string{metaOnly}},
		{"empty copy", []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			firstHome, firstProject := cbcHome(t)
			secondHome, secondProject := cbcHome(t)
			placeholder := filepath.Join(firstProject, "conv1.jsonl")
			usable := filepath.Join(secondProject, "conv1.jsonl")
			writeCBC(t, placeholder, tc.first...)
			writeCBC(t, usable, metaOnly, turn)
			older := time.Now().Add(-time.Hour)
			require.NoError(t, os.Chtimes(usable, older, older))
			legacyRoot := t.TempDir()
			manifest := writeCodeBuddyFixture(t, legacyRoot, "ws", "conv1")

			p := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy, IDPrefix: "codebuddy:"}).
				NewProvider(ProviderConfig{Roots: []string{firstHome, secondHome, legacyRoot}, Machine: "m"})
			for _, req := range []FindSourceRequest{
				{RawSessionID: "conv1"},
				{FullSessionID: "codebuddy:conv1"},
			} {
				source, found, err := p.FindSource(t.Context(), req)
				require.NoError(t, err)
				require.True(t, found)
				assert.Equal(t, usable, source.DisplayPath, "the copy with turns answers the lookup")
			}

			sources, err := p.Discover(t.Context())
			require.NoError(t, err)
			var legacy *SourceRef
			for i := range sources {
				if sources[i].DisplayPath == manifest {
					legacy = &sources[i]
				}
			}
			require.NotNil(t, legacy, "discovery still lists the 4.x manifest")
			outcome, err := p.Parse(t.Context(), ParseRequest{Source: *legacy})
			require.NoError(t, err)
			assert.Empty(t, outcome.Results, "the 4.x history yields to the copy with turns")
			assert.Equal(t, SkipNoSession, outcome.SkipReason)
		})
	}
}

// Streaming discovery and change events name the same copy of a conversation
// that several homes hold: the newer copy with turns. The 4.x manifest still
// streams after the transcripts.
func TestCodeBuddyStreamingDiscoveryYieldsPreferredCopy(t *testing.T) {
	line := `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hi"}]}`
	newerHome, newerProject := cbcHome(t)
	olderHome, olderProject := cbcHome(t)
	newer := filepath.Join(newerProject, "conv1.jsonl")
	older := filepath.Join(olderProject, "conv1.jsonl")
	writeCBC(t, newer, line)
	writeCBC(t, older, line)
	stale := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(older, stale, stale))
	manifest := filepath.Join(olderHome, "history", "ws", "conv2", "index.json")
	writeCBC(t, manifest, `{"messages":[]}`)

	p := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy, IDPrefix: "codebuddy:"}).
		NewProvider(ProviderConfig{Roots: []string{olderHome, newerHome}, Machine: "m"})
	var streamed []string
	err := p.(StreamingDiscoverer).DiscoverEach(t.Context(), func(source SourceRef) error {
		streamed = append(streamed, source.DisplayPath)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{newer, manifest}, streamed)

	for _, changed := range []string{older, newer} {
		sources, err := p.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: changed, EventKind: "write"})
		require.NoError(t, err)
		require.Len(t, sources, 1, changed)
		assert.Equal(t, newer, sources[0].DisplayPath, "a change to %s refreshes the preferred copy", changed)
	}
	require.NoError(t, os.Remove(older))
	sources, err := p.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: older, EventKind: "remove"})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, older, sources[0].DisplayPath, "a removed copy is still reported")
}
