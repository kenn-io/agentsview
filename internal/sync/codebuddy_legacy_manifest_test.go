package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A converted conversation whose 4.x manifest lists messages by id alone
// counts the request totals, and keeps them after the message file is gone.
func TestSyncCodeBuddyConvertedReadsIDOnlyManifest(t *testing.T) {
	home, project := cbcSyncHome(t)
	legacyRoot := t.TempDir()
	convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
	require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"), []byte(`{"messages":[{"id":"u1"},{"id":"a1"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "u1.json"), []byte(`{"role":"user","message":{"content":[{"type":"text","text":"old prompt"}]}}`), 0o600))
	reply := filepath.Join(convDir, "messages", "a1.json")
	require.NoError(t, os.WriteFile(reply, []byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req-a1","modelId":"glm-x","statsSnapshot":{"inputTokens":300,"outputTokens":40}}}`), 0o600))
	writeCBCSync(t, filepath.Join(project, "conv1.jsonl"),
		`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"old prompt"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"old reply"}]}`,
	)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home, legacyRoot)
	engine.SyncAll(t.Context(), nil)
	require.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens,
		"the reply's role comes from its message file")

	require.NoError(t, os.Remove(reply))
	engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens,
		"an unreadable message of unknown role keeps the archived total")
}
