package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rewinding a CodeBuddy conversation before its first message, after
// CodeBuddy's cleanup removed a saved tool output, leaves the whole old
// history in a branch and the conversation itself empty. The branch shows the
// output the conversation archived, and keeps it once the user continues.
func TestSyncCodeBuddyRewindBeforeFirstMessageKeepsRemovedOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync func(*testing.T, *Engine, string)
	}{
		{"full sync", func(t *testing.T, e *Engine, _ string) { e.SyncAll(t.Context(), nil) }},
		{"changed path", func(_ *testing.T, e *Engine, path string) { e.SyncPaths([]string{path}) }},
		{"single session", func(t *testing.T, e *Engine, _ string) {
			require.NoError(t, e.SyncSingleSessionContext(t.Context(), "codebuddy:s1"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testCodeBuddyRewindBeforeFirstMessageKeepsRemovedOutput(t, tc.sync)
		})
	}
}

func testCodeBuddyRewindBeforeFirstMessageKeepsRemovedOutput(
	t *testing.T, sync func(*testing.T, *Engine, string),
) {
	home, project := cbcSyncHome(t)
	transcript := filepath.Join(project, "s1.jsonl")
	side := filepath.Join(project, "s1", "tool-results", "c1.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(side), 0o755))
	require.NoError(t, os.WriteFile(side, []byte("the full output"), 0o600))
	turn := []string{
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"content":[{"type":"input_text","text":"go"}]}`,
		`{"id":"c1","parentId":"u1","type":"function_call","timestamp":1767225601000,"name":"Bash","callId":"c1","arguments":"{}"}`,
		`{"id":"r1","parentId":"c1","type":"function_call_result","timestamp":1767225602000,"name":"Bash","callId":"c1","output":"<persisted-output>\nOutput too large (1KB). Full output saved to: ` + filepath.ToSlash(side) + `\n</persisted-output>"}`,
		`{"id":"a1","parentId":"r1","type":"message","role":"assistant","timestamp":1767225603000,"content":[{"type":"output_text","text":"done"}]}`,
	}
	notice := `{"id":"n1","type":"resend-fork-notice","timestamp":1767225604000}`
	writeCBCSync(t, transcript, turn...)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	engine.SyncAll(t.Context(), nil)
	require.Equal(t, "the full output", cbcToolResult(t, database, "codebuddy:s1", "c1"))

	// CodeBuddy's cleanup removes the saved output, then the user rewinds
	// to before the first message and has not sent a new prompt yet.
	require.NoError(t, os.Remove(side))
	writeCBCSync(t, transcript, append(turn, notice)...)
	sync(t, engine, transcript)
	branch, err := database.GetSessionFull(t.Context(), "codebuddy:s1-u1")
	require.NoError(t, err)
	require.NotNil(t, branch, "the rewound history becomes a branch")
	assert.Equal(t, "the full output", cbcToolResult(t, database, "codebuddy:s1-u1", "c1"),
		"the branch keeps the output the emptied conversation archived")

	// The user continues; the conversation's new turn replaces its archive.
	writeCBCSync(t, transcript, append(turn, notice,
		`{"id":"u2","type":"message","role":"user","timestamp":1767225605000,"content":[{"type":"input_text","text":"again"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1767225606000,"content":[{"type":"output_text","text":"ok"}]}`,
	)...)
	sync(t, engine, transcript)
	assert.Equal(t, "the full output", cbcToolResult(t, database, "codebuddy:s1-u1", "c1"),
		"the branch still holds the output after the conversation moves on")
	assert.Empty(t, cbcToolResult(t, database, "codebuddy:s1", "c1"),
		"the continued conversation no longer holds the rewound call")
}

// When CodeBuddy's cleanup has removed a saved tool output and a rewind moves
// its call into a branch, a failed branch save leaves the conversation's
// archive holding the output, so the retry still shows it in the branch.
func TestSyncCodeBuddyFailedBranchSaveKeepsRemovedOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync func(*testing.T, *Engine, string)
	}{
		{"full sync", func(t *testing.T, e *Engine, _ string) { e.SyncAll(t.Context(), nil) }},
		{"changed path", func(_ *testing.T, e *Engine, path string) { e.SyncPaths([]string{path}) }},
		{"single session", func(t *testing.T, e *Engine, _ string) {
			_ = e.SyncSingleSessionContext(t.Context(), "codebuddy:s1")
		}},
		{"first import", func(t *testing.T, e *Engine, _ string) {
			_, _, _ = e.RunFirstImport(t.Context(), nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testCodeBuddyFailedBranchSaveKeepsRemovedOutput(t, tc.sync)
		})
	}
}

func testCodeBuddyFailedBranchSaveKeepsRemovedOutput(
	t *testing.T, sync func(*testing.T, *Engine, string),
) {
	home, project := cbcSyncHome(t)
	transcript := filepath.Join(project, "s1.jsonl")
	side := filepath.Join(project, "s1", "tool-results", "c1.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(side), 0o755))
	require.NoError(t, os.WriteFile(side, []byte("the full output"), 0o600))
	writeCBCSync(t, transcript, cbcPersistedTurn(side)...)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	engine.SyncAll(t.Context(), nil)
	require.Equal(t, "the full output", cbcToolResult(t, database, "codebuddy:s1", "c1"))

	// CodeBuddy's cleanup removes the saved output, then the user rewinds
	// the turn and continues; saving the new branch fails.
	require.NoError(t, os.Remove(side))
	writeCBCSync(t, transcript, append(cbcPersistedTurn(side), cbcRewind...)...)
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
	sync(t, engine, transcript)
	branch, err := database.GetSession(t.Context(), "codebuddy:s1-u1")
	require.NoError(t, err)
	require.Nil(t, branch, "the branch save fails")
	assert.Equal(t, "the full output", cbcToolResult(t, database, "codebuddy:s1", "c1"),
		"the conversation keeps the output until its branch is saved")

	_, err = raw.ExecContext(t.Context(), `DROP TRIGGER fail_codebuddy_branch_insert`)
	require.NoError(t, err)
	sync(t, engine, transcript)
	branch, err = database.GetSession(t.Context(), "codebuddy:s1-u1")
	require.NoError(t, err)
	require.NotNil(t, branch, "the retry saves the branch")
	assert.Equal(t, "the full output", cbcToolResult(t, database, "codebuddy:s1-u1", "c1"),
		"the branch shows the output the conversation kept")
	assert.Empty(t, cbcToolResult(t, database, "codebuddy:s1", "c1"),
		"the conversation no longer holds the rewound call")
}
