package parser

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeUsagePeerConversationIsNotExcluded(t *testing.T) {
	fixture := loadFixture(t, "claude/usage_peer_conversation.jsonl")
	lines := strings.Split(strings.TrimSpace(fixture), "\n")
	const peerText = "Another Claude session sent a message: Please review the retry logic."
	const queuedText = "<cross-session-message>Please review the retry logic.</cross-session-message>"
	queued := lines[0] + "\n" +
		`{"type":"attachment","timestamp":"2026-11-04T12:00:01Z","attachment":{"type":"queued_command","commandMode":"prompt","isMeta":true,"origin":{"kind":"peer"},"prompt":"` + queuedText + `"}}` + "\n" +
		strings.ReplaceAll(lines[2], `"parentUuid":"peer"`, `"parentUuid":"usage"`) + "\n"
	for _, tt := range []struct {
		name, content, peerText string
	}{
		{"non-meta peer", fixture, peerText},
		{"meta peer", strings.ReplaceAll(fixture, `"isMeta":false`, `"isMeta":true`), peerText},
		{"queued peer", queued, queuedText},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "project", "usage-peer.jsonl")
			writeSourceFile(t, path, tt.content)
			provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}, Machine: "local"})
			require.True(t, ok)
			source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "usage-peer"})
			require.NoError(t, err)
			require.True(t, found)
			out, err := provider.Parse(t.Context(), ParseRequest{
				Source: source, Fingerprint: SourceFingerprint{Key: path, Size: int64(len(tt.content))}, Machine: "local",
			})
			require.NoError(t, err)
			assert.Empty(t, out.ExcludedSessionIDs, "peer conversations must not enter archive deletion")
			require.Len(t, out.Results, 1)
			result := out.Results[0].Result
			assert.Equal(t, "usage-peer", result.Session.ID)
			assert.Equal(t, 1, result.Session.UserMessageCount)
			assert.Empty(t, result.Session.FirstMessage)
			require.Len(t, result.Messages, 3)
			assert.Equal(t, "/usage", result.Messages[0].Content)
			assert.False(t, result.Messages[0].IsSystem)
			assert.Equal(t, "peer_message", result.Messages[1].SourceSubtype)
			assert.True(t, result.Messages[1].IsSystem)
			assert.Equal(t, tt.peerText, result.Messages[1].Content)
			assert.Equal(t, "The retry preserves the original request.", result.Messages[2].Content)
		})
	}
}
