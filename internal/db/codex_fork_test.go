package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexForkStateBeforeProvenanceSchema(t *testing.T) {
	// A rebuild opens the old archive read-only, before migrations. Model the
	// shipped schema without the new, optional fork provenance table.
	old, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "old.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, old.Close()) })
	_, err = old.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	d := &DB{}
	d.reader.Store(old)
	state, err := d.GetCodexForkState(t.Context(), "codex:child")
	require.NoError(t, err)
	assert.Nil(t, state)
}

func TestCodexForkProvenanceFailureRollsBackContent(t *testing.T) {
	for _, route := range []string{"batch", "staged"} {
		t.Run(route, func(t *testing.T) {
			database := testDB(t)
			write := SessionBatchWrite{
				Session:         Session{ID: "codex:child", Agent: "codex", Project: "project", Machine: "local"},
				Messages:        []Message{{SessionID: "codex:child", Role: "assistant", Content: "saved answer"}},
				ReplaceMessages: true, DataVersion: CurrentDataVersion() - 1,
				CodexForkState: &CodexForkState{ParentID: "parent"},
			}
			result, err := database.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{write})
			require.NoError(t, err)
			require.Equal(t, 1, result.WrittenSessions)
			require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER reject_fork_provenance BEFORE UPDATE ON codex_fork_state
		 BEGIN SELECT RAISE(FAIL, 'reject fork provenance'); END`)
				return err
			}))
			write.Messages[0].Content = "must roll back"
			write.CodexForkState = &CodexForkState{ParentID: "parent", ParentKnown: true}
			write.DataVersion = CurrentDataVersion()
			if route == "staged" {
				staged := newScratchStagedResults(t)
				defer staged.Close()
				require.Error(t, database.WriteSessionBatchStaged(t.Context(), write, staged, nil, nil))
			} else {
				result, err = database.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{write})
				require.NoError(t, err)
				assert.Equal(t, 1, result.FailedSessions)
			}
			messages, err := database.GetAllMessages(t.Context(), "codex:child")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "saved answer", messages[0].Content)
			state, err := database.GetCodexForkState(t.Context(), "codex:child")
			require.NoError(t, err)
			require.NotNil(t, state)
			assert.False(t, state.ParentKnown)
			assert.Less(t, database.GetSessionDataVersion(t.Context(), "codex:child"), CurrentDataVersion())
		})
	}
}

func TestListUnresolvedCodexForkSources(t *testing.T) {
	database := testDB(t)
	unresolvedPath := filepath.Join(t.TempDir(), "unresolved.jsonl")
	resolvedPath := filepath.Join(t.TempDir(), "resolved.jsonl")
	trashedPath := filepath.Join(t.TempDir(), "trashed.jsonl")

	writes := []SessionBatchWrite{
		{
			Session:        Session{ID: "codex:unresolved", Agent: "codex", Project: "project", Machine: "local", FilePath: &unresolvedPath},
			DataVersion:    CurrentDataVersion() - 1,
			CodexForkState: &CodexForkState{ParentID: "parent-a"},
		},
		{
			Session:        Session{ID: "codex:resolved", Agent: "codex", Project: "project", Machine: "local", FilePath: &resolvedPath},
			DataVersion:    CurrentDataVersion(),
			CodexForkState: &CodexForkState{ParentID: "parent-b", ParentKnown: true},
		},
		{
			Session:        Session{ID: "codex:trashed", Agent: "codex", Project: "project", Machine: "local", FilePath: &trashedPath},
			DataVersion:    CurrentDataVersion() - 1,
			CodexForkState: &CodexForkState{ParentID: "parent-c"},
		},
		{
			Session:        Session{ID: "codex:no-path", Agent: "codex", Project: "project", Machine: "local"},
			DataVersion:    CurrentDataVersion() - 1,
			CodexForkState: &CodexForkState{ParentID: "parent-d"},
		},
	}
	res, err := database.WriteSessionBatchContext(t.Context(), writes)
	require.NoError(t, err)
	require.Equal(t, len(writes), res.WrittenSessions)
	require.NoError(t, database.SoftDeleteSession(t.Context(), "codex:trashed"))

	got, err := database.ListUnresolvedCodexForkSources(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []UnresolvedCodexForkSource{{
		Agent:    "codex",
		FilePath: unresolvedPath,
		ParentID: "parent-a",
	}}, got)
}
