package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// cbcPersistedTurn is one CodeBuddy turn whose tool output was saved to side.
func cbcPersistedTurn(side string) []string {
	return []string{
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"content":[{"type":"input_text","text":"go"}]}`,
		`{"id":"c1","parentId":"u1","type":"function_call","timestamp":1767225601000,"name":"Bash","callId":"c1","arguments":"{}"}`,
		`{"id":"r1","parentId":"c1","type":"function_call_result","timestamp":1767225602000,"name":"Bash","callId":"c1","output":"<persisted-output>\nOutput too large (1KB). Full output saved to: ` + filepath.ToSlash(side) + `\n</persisted-output>"}`,
		`{"id":"a1","parentId":"r1","type":"message","role":"assistant","timestamp":1767225603000,"content":[{"type":"output_text","text":"done"}]}`,
	}
}

// cbcRewind rewinds the turn before it and sends a new prompt.
var cbcRewind = []string{
	`{"id":"n1","type":"resend-fork-notice","timestamp":1767225604000}`,
	`{"id":"u2","type":"message","role":"user","timestamp":1767225605000,"content":[{"type":"input_text","text":"again"}]}`,
	`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1767225606000,"content":[{"type":"output_text","text":"ok"}]}`,
}

// A conversation saved from one configured home can later be read from a
// newer copy under another home. When CodeBuddy's cleanup has removed a saved
// tool output and the newer copy rewinds that call into a branch, the branch
// shows the output the archive saved from the older copy.
func TestSyncCodeBuddyNewerCopyRewindKeepsRemovedOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync func(*testing.T, *Engine, string)
	}{
		{"full sync", func(t *testing.T, e *Engine, _ string) { e.SyncAll(t.Context(), nil) }},
		{"changed path", func(_ *testing.T, e *Engine, path string) { e.SyncPaths([]string{path}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			olderHome, olderProject := cbcSyncHome(t)
			newerHome, newerProject := cbcSyncHome(t)
			older := filepath.Join(olderProject, "s1.jsonl")
			newer := filepath.Join(newerProject, "s1.jsonl")
			side := filepath.Join(olderProject, "s1", "tool-results", "c1.txt")
			require.NoError(t, os.MkdirAll(filepath.Dir(side), 0o755))
			require.NoError(t, os.WriteFile(side, []byte("the full output"), 0o600))
			writeCBCSync(t, older, cbcPersistedTurn(side)...)
			stale := time.Now().Add(-time.Hour)
			require.NoError(t, os.Chtimes(older, stale, stale))

			database := openTestDB(t)
			engine := newCBCEngine(t, database, olderHome, newerHome)
			engine.SyncAll(t.Context(), nil)
			require.Equal(t, "the full output", cbcToolResult(t, database, "codebuddy:s1", "c1"))

			// CodeBuddy's cleanup removes the saved output; the newer copy
			// holds the same turn, rewound into a branch.
			require.NoError(t, os.Remove(side))
			newerSide := filepath.Join(newerProject, "s1", "tool-results", "c1.txt")
			writeCBCSync(t, newer, append(cbcPersistedTurn(newerSide), cbcRewind...)...)
			tc.sync(t, engine, newer)

			sess, err := database.GetSessionFull(t.Context(), "codebuddy:s1")
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.NotNil(t, sess.FilePath)
			assert.Equal(t, newer, *sess.FilePath, "the newer copy owns the conversation")
			branch, err := database.GetSessionFull(t.Context(), "codebuddy:s1-u1")
			require.NoError(t, err)
			require.NotNil(t, branch, "the rewound turn becomes a branch")
			assert.Equal(t, "the full output", cbcToolResult(t, database, "codebuddy:s1-u1", "c1"),
				"the branch keeps the output saved from the older copy")
			assert.Empty(t, cbcToolResult(t, database, "codebuddy:s1", "c1"),
				"the conversation no longer holds the rewound call")
		})
	}
}

// When saving one branch of a CodeBuddy conversation fails, the next sync
// saves it even though the transcript has not changed since.
func TestSyncCodeBuddyFailedBranchSaveRetries(t *testing.T) {
	home, project := cbcSyncHome(t)
	transcript := filepath.Join(project, "s1.jsonl")
	writeCBCSync(t, transcript, append(cbcPersistedTurn(filepath.Join(project, "s1", "tool-results", "c1.txt")), cbcRewind...)...)
	database := openTestDB(t)
	raw, err := sql.Open("sqlite3", database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	_, err = raw.ExecContext(t.Context(), `
		CREATE TRIGGER fail_codebuddy_branch_insert
		BEFORE INSERT ON sessions
		WHEN NEW.id = 'codebuddy:s1-u1'
		BEGIN
			SELECT RAISE(FAIL, 'injected CodeBuddy branch write failure');
		END;
	`)
	require.NoError(t, err)

	engine := newCBCEngine(t, database, home)
	failed := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, failed.Failed)
	main, err := database.GetSession(t.Context(), "codebuddy:s1")
	require.NoError(t, err)
	// A first import has no archived output to protect, so the conversation
	// still saves after its branch fails.
	require.NotNil(t, main, "the conversation saves although its branch failed")
	branch, err := database.GetSession(t.Context(), "codebuddy:s1-u1")
	require.NoError(t, err)
	require.Nil(t, branch)

	_, err = raw.ExecContext(t.Context(), `DROP TRIGGER fail_codebuddy_branch_insert`)
	require.NoError(t, err)
	retry := engine.SyncAll(t.Context(), nil)
	assert.Zero(t, retry.Failed)
	branch, err = database.GetSession(t.Context(), "codebuddy:s1-u1")
	require.NoError(t, err)
	require.NotNil(t, branch, "the unchanged transcript retries the failed branch")
	for _, id := range []string{"codebuddy:s1", "codebuddy:s1-u1"} {
		assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), id), id)
	}
	_, hasDigest, err := database.GetProviderStatHash(t.Context(), parser.AgentCodeBuddy, transcript)
	require.NoError(t, err)
	assert.True(t, hasDigest, "a complete save marks the transcript current")
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced, "a current transcript is not saved again")
}

// A branch the user moved to trash does not keep its conversation from being
// marked current.
func TestSyncCodeBuddyTrashedBranchLeavesConversationCurrent(t *testing.T) {
	home, project := cbcSyncHome(t)
	transcript := filepath.Join(project, "s1.jsonl")
	lines := append(cbcPersistedTurn(filepath.Join(project, "s1", "tool-results", "c1.txt")), cbcRewind...)
	writeCBCSync(t, transcript, lines...)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	engine.SyncAll(t.Context(), nil)
	require.NoError(t, database.SoftDeleteSession(t.Context(), "codebuddy:s1-u1"))

	writeCBCSync(t, transcript, append(lines,
		`{"id":"u3","parentId":"a2","type":"message","role":"user","timestamp":1767225607000,"content":[{"type":"input_text","text":"more"}]}`,
	)...)
	synced := engine.SyncAll(t.Context(), nil)
	assert.Zero(t, synced.Failed)
	assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), "codebuddy:s1"))
	assert.True(t, database.IsSessionTrashed(t.Context(), "codebuddy:s1-u1"), "the branch stays in trash")
	_, hasDigest, err := database.GetProviderStatHash(t.Context(), parser.AgentCodeBuddy, transcript)
	require.NoError(t, err)
	assert.True(t, hasDigest, "the trashed branch counts as resolved")
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced, "a current transcript is not saved again")
}
