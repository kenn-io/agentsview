package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A 4.x manifest may list messages by id alone; the 4.x reader takes each
// role from the message file, so the converted conversation does too.
func TestCodeBuddyConvertedReadsTotalsFromIDOnlyManifest(t *testing.T) {
	for _, tc := range []struct {
		name, manifest string
		writeReply     bool
		wantUsage      bool
		wantIncomplete bool
	}{
		{
			name:      "role missing",
			manifest:  `{"messages":[{"id":"u1"},{"id":"a1"}]}`,
			wantUsage: true, writeReply: true,
		},
		{
			name:      "role empty",
			manifest:  `{"messages":[{"id":"u1","role":""},{"id":"a1","role":""}]}`,
			wantUsage: true, writeReply: true,
		},
		{
			name:           "message file missing",
			manifest:       `{"messages":[{"id":"u1"},{"id":"a1"}]}`,
			wantIncomplete: true,
		},
		{
			name:       "listed as user",
			manifest:   `{"messages":[{"id":"u1"},{"id":"a1","role":"user"}]}`,
			writeReply: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcHome(t)
			legacyRoot := t.TempDir()
			convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
			require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"), []byte(tc.manifest), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "u1.json"), []byte(`{"role":"user","message":{"content":[{"type":"text","text":"go"}]}}`), 0o600))
			if tc.writeReply {
				require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "a1.json"), []byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","modelId":"luna","statsSnapshot":{"inputTokens":300,"outputTokens":40}}}`), 0o600))
			}
			writeCBC(t, filepath.Join(project, "conv1.jsonl"),
				`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
				`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"go"}]}`,
				`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"done"}],"providerData":{"conversationRequestId":"req1"}}`,
			)
			main := cbcSession(t, parseCBCHome(t, home, legacyRoot), "codebuddy:conv1")
			assert.Equal(t, tc.wantIncomplete, main.Session.LegacyCompanionIncomplete)
			require.Len(t, main.Messages, 2)
			reply := main.Messages[1]
			if tc.wantUsage {
				assert.Equal(t, "cb4:req1:a1", reply.SourceUUID)
				assert.Equal(t, 40, reply.OutputTokens)
			} else {
				assert.NotEqual(t, "cb4:req1:a1", reply.SourceUUID)
				assert.Zero(t, reply.OutputTokens)
			}
		})
	}
}
