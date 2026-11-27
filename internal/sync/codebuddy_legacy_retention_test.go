package sync

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Deleting an older converted conversation's 4.x folder keeps the request
// totals and pasted images the archive read from it, through a sync, a
// forced parse, and a rebuild. Once a 4.x folder returns, its corrected
// totals win.
func TestSyncCodeBuddyConvertedKeepsLegacyDataAfterFolderDeleted(t *testing.T) {
	home, project := cbcSyncHome(t)
	legacyRoot := t.TempDir()
	convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
	writeLegacy := func(output int) {
		t.Helper()
		writeCodeBuddyLegacyUsage(t, convDir, map[string]int{"a1": output})
		png, err := base64.StdEncoding.DecodeString(codebuddyBlobPNG)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(convDir, "assets"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(convDir, "assets", "pic.png"), png, 0o600))
	}
	writeLegacy(40)
	writeCBCSync(t, filepath.Join(project, "conv1.jsonl"),
		`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image":"codebuddy-asset://assets/pic.png"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"a pixel"}]}`,
	)
	database := openTestDB(t)
	database.SetAssetsDir(t.TempDir())
	engine := newCBCEngine(t, database, home, legacyRoot)
	prompt := func() string {
		t.Helper()
		msgs, err := database.GetMessages(t.Context(), "codebuddy:conv1", 0, 100, true)
		require.NoError(t, err)
		require.NotEmpty(t, msgs)
		return msgs[0].Content
	}
	engine.SyncAll(t.Context(), nil)
	require.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens)
	link := cbcAssetLinkRe.FindString(prompt())
	require.NotEmpty(t, link, "the first sync archives the image: %q", prompt())

	require.NoError(t, os.RemoveAll(convDir))
	for _, step := range []struct {
		name string
		run  func()
	}{
		{"sync", func() { engine.SyncAll(t.Context(), nil) }},
		{"forced parse", func() { engine.SyncAllForceParse(t.Context(), nil) }},
		{"rebuild", func() {
			st := engine.ResyncAll(t.Context(), nil)
			require.False(t, st.Aborted, "%v", st.Warnings)
		}},
	} {
		step.run()
		assert.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens,
			"%s keeps the archived 4.x totals", step.name)
		got := prompt()
		assert.Contains(t, got, link, "%s keeps the archived image", step.name)
		assert.NotContains(t, got, "[image unavailable", step.name)
		sess, err := database.GetSessionFull(t.Context(), "codebuddy:conv1")
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Equal(t, 40, sess.TotalOutputTokens, step.name)
		assert.Equal(t, 1, sess.UserMessageCount, step.name)
	}

	writeLegacy(45)
	engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 45, cbcDailyTotals(t, database).OutputTokens,
		"a returned 4.x folder's corrected totals win")
	assert.Contains(t, prompt(), link)
}
