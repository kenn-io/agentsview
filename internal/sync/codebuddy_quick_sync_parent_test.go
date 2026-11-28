package sync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A quick sync links an unchanged branch to its parent when the parent
// arrives under a filename that differs from the session id CodeBuddy
// recorded for it. Only the parent's transcript is new; the branch's own
// files and the catalog keep their old times.
func TestSyncAllSinceCodeBuddyBranchLinksParentArrivingUnderOtherFilename(t *testing.T) {
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
	cutoff := backdateCBCTrees(t, home)

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
	requireQuickSyncSkipsUnchanged(t, engine, cutoff)

	writeCBCSync(t, filepath.Join(project, "parent-file.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1767225500000,"sessionId":"run-p","content":[{"type":"input_text","text":"first"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225501000,"content":[{"type":"output_text","text":"hi"}]}`,
	)
	engine.SyncAllSince(t.Context(), cutoff, nil)
	assert.Equal(t, "codebuddy:parent-file", parent(), "the branch links to the parent's archive id")
}
