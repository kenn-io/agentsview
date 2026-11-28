package sync

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// cbcNextTurn continues cbcPersistedTurn with a second prompt and reply.
var cbcNextTurn = []string{
	`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1767225604000,"content":[{"type":"input_text","text":"next"}]}`,
	`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1767225605000,"content":[{"type":"output_text","text":"next reply"}]}`,
}

// cbcHasContent reports whether a stored session has a message with content.
func cbcHasContent(t *testing.T, database *db.DB, sessionID, content string) bool {
	t.Helper()
	msgs, err := database.GetMessages(t.Context(), sessionID, 0, 100, true)
	require.NoError(t, err)
	for _, m := range msgs {
		if m.Content == content {
			return true
		}
	}
	return false
}

// A CodeBuddy conversation whose saved-output folder cannot be read is still
// imported and keeps gaining turns. Its saved outputs show placeholders, or
// the text the archive kept, until the folder is readable again.
func TestSyncCodeBuddyUnreadableSavedOutputFolder(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not block reads here")
	}
	for _, tc := range []struct {
		name         string
		blockedFirst bool
		want         string
	}{
		{"unreadable before import", true, "persisted-output"},
		{"unreadable after import", false, "the full output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcSyncHome(t)
			transcript := filepath.Join(project, "s1.jsonl")
			dir := filepath.Join(project, "s1", "tool-results")
			side := filepath.Join(dir, "c1.txt")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(side, []byte("the full output"), 0o600))
			lines := cbcPersistedTurn(side)
			writeCBCSync(t, transcript, lines...)
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			database := openTestDB(t)
			engine := newCBCEngine(t, database, home)
			if tc.blockedFirst {
				require.NoError(t, os.Chmod(dir, 0o000))
			}
			engine.SyncAll(t.Context(), nil)
			sess, err := database.GetSession(t.Context(), "codebuddy:s1")
			require.NoError(t, err)
			require.NotNil(t, sess, "the conversation is imported")
			if tc.blockedFirst {
				assert.Contains(t, cbcToolResult(t, database, "codebuddy:s1", "c1"), "persisted-output",
					"the unreadable output shows its placeholder")
			} else {
				require.Equal(t, "the full output", cbcToolResult(t, database, "codebuddy:s1", "c1"))
				require.NoError(t, os.Chmod(dir, 0o000))
			}

			writeCBCSync(t, transcript, append(lines, cbcNextTurn...)...)
			engine.SyncAll(t.Context(), nil)
			assert.True(t, cbcHasContent(t, database, "codebuddy:s1", "next reply"),
				"the conversation gains the new turn")
			assert.Contains(t, cbcToolResult(t, database, "codebuddy:s1", "c1"), tc.want)

			require.NoError(t, os.Chmod(dir, 0o755))
			engine.SyncAll(t.Context(), nil)
			assert.Equal(t, "the full output", cbcToolResult(t, database, "codebuddy:s1", "c1"),
				"the readable folder shows the saved output")
		})
	}
}

// A branch imported before its parent links to the parent once the parent
// arrives under a filename that differs from the session id CodeBuddy
// recorded for it, on an ordinary sync.
func TestSyncCodeBuddyBranchLinksParentArrivingUnderOtherFilename(t *testing.T) {
	home, project := cbcSyncHome(t)
	writeCBCSync(t, filepath.Join(project, "branch-file.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"sessionId":"run-b","content":[{"type":"input_text","text":"branch"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225601000,"content":[{"type":"output_text","text":"ok"}]}`,
	)
	conn := openCBCCatalog(t, home)
	_, err := conn.ExecContext(t.Context(), `CREATE TABLE conversations (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', custom_title TEXT, runtime_session_id TEXT, fork_source_id TEXT);
		INSERT INTO conversations VALUES ('c-parent', 'Parent', NULL, 'run-p', NULL);
		INSERT INTO conversations VALUES ('c-b', 'Branch', NULL, 'run-b', 'c-parent')`)
	require.NoError(t, err)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	parent := func() string {
		t.Helper()
		sess, err := database.GetSession(t.Context(), "codebuddy:branch-file")
		require.NoError(t, err)
		require.NotNil(t, sess)
		require.NotNil(t, sess.ParentSessionID)
		return *sess.ParentSessionID
	}
	engine.SyncAll(t.Context(), nil)
	require.Equal(t, "codebuddy:run-p", parent(), "without its parent the branch names the recorded id")

	writeCBCSync(t, filepath.Join(project, "parent-file.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1767225500000,"sessionId":"run-p","content":[{"type":"input_text","text":"first"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225501000,"content":[{"type":"output_text","text":"hi"}]}`,
	)
	engine.SyncAll(t.Context(), nil)
	assert.Equal(t, "codebuddy:parent-file", parent(), "the branch links to the parent's archive id")
}
