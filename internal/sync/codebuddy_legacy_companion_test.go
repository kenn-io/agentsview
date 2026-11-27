package sync

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeCodeBuddyLegacyUsage writes a converted conversation's 4.x directory
// with one assistant message per request total, listed in its manifest.
func writeCodeBuddyLegacyUsage(t *testing.T, convDir string, outputs map[string]int) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
	manifest := `{"messages":[`
	first := true
	for _, id := range []string{"a1", "a2"} {
		output, ok := outputs[id]
		if !ok {
			continue
		}
		if !first {
			manifest += ","
		}
		first = false
		manifest += `{"id":"` + id + `","role":"assistant"}`
		require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", id+".json"),
			[]byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req-`+id+`","modelId":"glm-x","statsSnapshot":{"inputTokens":300,"outputTokens":`+strconv.Itoa(output)+`}}}`), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"), []byte(manifest+"]}"), 0o600))
}

// A converted conversation imported before its 4.x directory was available
// gains the 4.x request totals once the directory appears or changes, without
// a forced sync or a change to the transcript.
func TestSyncCodeBuddyConvertedConversationFollowsLegacyCompanion(t *testing.T) {
	home, project := cbcSyncHome(t)
	legacyRoot := t.TempDir()
	historyWS := filepath.Join(legacyRoot, "history", "ws")
	require.NoError(t, os.MkdirAll(historyWS, 0o755))
	transcript := filepath.Join(project, "conv1.jsonl")
	writeCBCSync(t, transcript,
		`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"old prompt"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"old reply"}]}`,
		`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1767225700000,"content":[{"type":"input_text","text":"continued in 5.x"}]}`,
	)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home, legacyRoot)
	engine.SyncAll(t.Context(), nil)
	assert.Zero(t, cbcDailyTotals(t, database).OutputTokens, "no 4.x totals before the directory exists")
	// A warm pass records the stat digest the next pass compares against.
	engine.SyncAll(t.Context(), nil)

	convDir := filepath.Join(historyWS, "conv1")
	writeCodeBuddyLegacyUsage(t, convDir, map[string]int{"a1": 40})
	engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens,
		"a plain sync reads the 4.x directory once it appears")
	sess, err := database.GetSessionFull(t.Context(), "codebuddy:conv1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.FilePath)
	assert.Equal(t, transcript, *sess.FilePath, "the transcript still owns the conversation")

	// A changed 4.x message reaches the converted conversation through its
	// change event.
	writeCodeBuddyLegacyUsage(t, convDir, map[string]int{"a1": 40, "a2": 5})
	engine.SyncPaths([]string{filepath.Join(convDir, "messages", "a2.json")})
	assert.Equal(t, 45, cbcDailyTotals(t, database).OutputTokens,
		"a 4.x change event refreshes the converted conversation")
}
