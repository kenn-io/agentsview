package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Correcting a 4.x request total in place, keeping the file's size and
// modification time, reaches the converted conversation on a plain sync.
func TestSyncCodeBuddyConvertedConversationFollowsInPlaceTotalsEdit(t *testing.T) {
	home, project := cbcSyncHome(t)
	legacyRoot := t.TempDir()
	convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
	writeCodeBuddyLegacyUsage(t, convDir, map[string]int{"a1": 40})
	writeCBCSync(t, filepath.Join(project, "conv1.jsonl"),
		`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"old prompt"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"old reply"}]}`,
	)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home, legacyRoot)
	engine.SyncAll(t.Context(), nil)
	require.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens)
	engine.SyncAll(t.Context(), nil)

	message := filepath.Join(convDir, "messages", "a1.json")
	info, err := os.Stat(message)
	require.NoError(t, err)
	body, err := os.ReadFile(message)
	require.NoError(t, err)
	edited := strings.Replace(string(body), `"outputTokens":40`, `"outputTokens":45`, 1)
	require.Len(t, edited, len(body))
	require.NoError(t, os.WriteFile(message, []byte(edited), 0o600))
	require.NoError(t, os.Chtimes(message, info.ModTime(), info.ModTime()))

	engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 45, cbcDailyTotals(t, database).OutputTokens,
		"a plain sync shows the corrected 4.x total")
}
