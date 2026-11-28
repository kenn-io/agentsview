package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A 4.x request recorded with zero tokens, whose reply the converted
// transcript lacks, keeps a hidden usage row under its cb4: identity.
func TestCodeBuddyConvertedKeepsZeroLegacyTotalWithoutReply(t *testing.T) {
	home, project := cbcHome(t)
	legacyRoot := t.TempDir()
	convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
	require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"), []byte(`{"messages":[{"id":"a1","role":"assistant"},{"id":"a2","role":"assistant"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "a1.json"), []byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","modelId":"glm-x","statsSnapshot":{"inputTokens":300,"outputTokens":40}}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "a2.json"), []byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:03Z","extra":{"requestId":"req2","modelId":"glm-x","statsSnapshot":{"inputTokens":0,"outputTokens":0}}}`), 0o600))
	writeCBC(t, filepath.Join(project, "conv1.jsonl"),
		`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"go"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"done"}]}`,
	)

	main := cbcSession(t, parseCBCHome(t, home, legacyRoot), "codebuddy:conv1")
	var zero *ParsedMessage
	for i := range main.Messages {
		if main.Messages[i].SourceUUID == "cb4:req2:a2" {
			zero = &main.Messages[i]
		}
	}
	require.NotNil(t, zero, "the zero total keeps its request identity")
	assert.True(t, zero.IsSystem)
	assert.Empty(t, zero.Content)
	assert.Equal(t, "glm-x", zero.Model)
	assert.Zero(t, zero.OutputTokens)
	assert.True(t, zero.HasOutputTokens, "the zero is a known count")
	assert.False(t, zero.HasContextTokens,
		"a request total without its last step's prompt has no known context window")
	assert.Equal(t, 40, main.Session.TotalOutputTokens)
}
