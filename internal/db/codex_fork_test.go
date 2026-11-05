package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
