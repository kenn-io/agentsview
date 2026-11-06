package parser

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeProviderPeerUserMessages(t *testing.T) {
	// Synthetic records based on the Claude Code 2.1.310 artifact in #2718.
	const peerText = "Another Claude session sent a message:\n<cross-session-message from=\"uds:/tmp/peer.sock\" from-name=\"peer-a\" from-mode=\"prompting\">\nPING\n</cross-session-message>\n\nThis came from another Claude session."
	const humanText = "Explain <cross-session-message> in the documentation."
	const initial = "{\"type\":\"agent-setting\",\"entrypoint\":\"cli\"}\n"
	for _, blocks := range []bool{false, true} {
		for _, meta := range []bool{false, true} {
			t.Run(fmt.Sprintf("blocks=%t/meta=%t", blocks, meta), func(t *testing.T) {
				var content any = peerText
				if blocks {
					content = []map[string]string{{"type": "text", "text": peerText}}
				}
				encoded, err := json.Marshal(content)
				require.NoError(t, err)
				appended := fmt.Sprintf(`{"type":"user","uuid":"peer-1","isMeta":%t,"promptSource":"system","turnOrigin":"peer","origin":{"kind":"peer","name":"peer-a","body":"PING"},"message":{"role":"user","content":%s}}`, meta, encoded) + "\n" +
					`{"type":"user","uuid":"notice-1","parentUuid":"peer-1","isMeta":true,"promptSource":"system","message":{"content":"[Cross-session delivery notice]"}}` + "\n" +
					fmt.Sprintf(`{"type":"user","uuid":"human-1","parentUuid":"notice-1","message":{"content":%q}}`, humanText) + "\n" +
					`{"type":"assistant","uuid":"reply-1","parentUuid":"human-1","message":{"role":"assistant","content":"It wraps a peer message."}}` + "\n"
				root := t.TempDir()
				path := filepath.Join(root, "project", "peer.jsonl")
				writeSourceFile(t, path, initial+appended)
				provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}, Machine: "local"})
				require.True(t, ok)
				source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "peer"})
				require.NoError(t, err)
				require.True(t, found)
				fingerprint := SourceFingerprint{Key: path, Size: int64(len(initial) + len(appended))}
				for _, incremental := range []bool{false, true} {
					t.Run(fmt.Sprintf("incremental=%t", incremental), func(t *testing.T) {
						var messages []ParsedMessage
						if incremental {
							out, status, err := provider.ParseIncremental(t.Context(), IncrementalRequest{
								Source: source, Fingerprint: fingerprint, SessionID: "peer",
								Offset: int64(len(initial)), StoredEntrypoint: "cli",
							})
							require.NoError(t, err)
							require.Equal(t, IncrementalApplied, status)
							assert.Equal(t, int64(len(appended)), out.ConsumedBytes)
							assert.Equal(t, 1, out.UserMessageCount)
							messages = out.Messages
						} else {
							out, err := provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: fingerprint, Machine: "local"})
							require.NoError(t, err)
							require.Len(t, out.Results, 1)
							assert.Equal(t, humanText, out.Results[0].Result.Session.FirstMessage)
							assert.Equal(t, 1, out.Results[0].Result.Session.UserMessageCount)
							messages = out.Results[0].Result.Messages
						}
						require.Len(t, messages, 3)
						assert.Equal(t, peerText, messages[0].Content)
						assert.Equal(t, RoleUser, messages[0].Role)
						assert.True(t, messages[0].IsSystem)
						assert.Equal(t, "system", messages[0].SourceType)
						assert.Equal(t, "peer_message", messages[0].SourceSubtype)
						assert.Equal(t, "peer-1", messages[0].SourceUUID)
						assert.Equal(t, "system", messages[0].PromptSource)
						assert.Equal(t, humanText, messages[1].Content)
						assert.False(t, messages[1].IsSystem)
						assert.Equal(t, "It wraps a peer message.", messages[2].Content)
						for ordinal, message := range messages {
							assert.Equal(t, ordinal, message.Ordinal)
						}
					})
				}
			})
		}
	}
}

func TestClaudePeerRewindKeepsRetryIdentity(t *testing.T) {
	var transcript strings.Builder
	transcript.WriteString("{\"type\":\"user\",\"uuid\":\"root\",\"message\":{\"content\":\"start\"}}\n")
	parent := "root"
	for i := range 4 {
		fmt.Fprintf(&transcript, `{"type":"user","uuid":"peer-%d","parentUuid":%q,"isMeta":true,"origin":{"kind":"peer"},"message":{"content":"Another Claude session sent a message: PING"}}`+"\n", i, parent)
		fmt.Fprintf(&transcript, `{"type":"assistant","uuid":"reply-%d","parentUuid":"peer-%d","message":{"content":"PONG"}}`+"\n", i, i)
		parent = fmt.Sprintf("reply-%d", i)
	}
	transcript.WriteString("{\"type\":\"user\",\"uuid\":\"retry\",\"parentUuid\":\"root\",\"message\":{\"content\":\"try again\"}}\n")
	path := createTestFile(t, "rewind.jsonl", transcript.String())
	results, err := parseClaudeSession(path, "project", "local")
	require.NoError(t, err)
	require.Len(t, results, 1, "peer messages must not create a human-turn fork")
	assert.Equal(t, "rewind", results[0].Session.ID)
	assert.Equal(t, 2, results[0].Session.UserMessageCount)
	require.Len(t, results[0].Messages, 2)
	assert.Equal(t, "try again", results[0].Messages[1].Content)
}
