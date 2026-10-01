package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeSubagentSourcesCommitWithMessages(t *testing.T) {
	database := testDB(t)
	write := SessionBatchWrite{
		Session:               Session{ID: "agent-reviewer", Project: "project", Agent: "claude", Machine: "local"},
		Messages:              []Message{{SessionID: "agent-reviewer", Ordinal: 0, Role: "user", Content: "original"}},
		ClaudeSubagentSources: []string{"/sources/first.jsonl", "/sources/second.jsonl"},
		ReplaceMessages:       true,
	}
	result, err := database.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	require.Equal(t, 1, result.WrittenSessions)
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER reject_source_provenance
			BEFORE INSERT ON claude_subagent_sources WHEN NEW.file_path = '/sources/rejected.jsonl'
			BEGIN SELECT RAISE(FAIL, 'reject source provenance'); END`)
		return err
	}))
	write.Messages[0].Content = "must roll back"
	write.ClaudeSubagentSources = []string{"/sources/first.jsonl", "/sources/rejected.jsonl"}
	result, err = database.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	require.Equal(t, 1, result.FailedSessions)
	paths, err := database.GetClaudeSubagentSources(t.Context(), "agent-reviewer")
	require.NoError(t, err)
	assert.Equal(t, []string{"/sources/first.jsonl", "/sources/second.jsonl"}, paths)
	messages, err := database.GetAllMessages(t.Context(), "agent-reviewer")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "original", messages[0].Content)

	write.ReplaceMessages = false
	write.Messages[0].Ordinal = 1
	write.Messages[0].Content = "appended"
	write.ClaudeSubagentSources = []string{"/sources/third.jsonl"}
	result, err = database.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	require.Equal(t, 1, result.WrittenSessions)
	paths, err = database.GetClaudeSubagentSources(t.Context(), "agent-reviewer")
	require.NoError(t, err)
	assert.Equal(t, []string{"/sources/first.jsonl", "/sources/second.jsonl", "/sources/third.jsonl"}, paths)

	write.ReplaceMessages = true
	write.Messages[0].Ordinal = 0
	write.ClaudeSubagentSources = []string{"/sources/first.jsonl"}
	result, err = database.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	require.Equal(t, 1, result.WrittenSessions)
	paths, err = database.GetClaudeSubagentSources(t.Context(), "agent-reviewer")
	require.NoError(t, err)
	assert.Empty(t, paths, "a complete one-file replacement no longer depends on companions")
}

func TestClaudeSubagentSourcesWritableUpgradePreservesHistory(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.UpsertSession(t.Context(), Session{
		ID: "existing", Project: "project", Agent: "claude",
	}))
	require.NoError(t, database.ReplaceSessionMessages(t.Context(), "existing", []Message{{
		SessionID: "existing", Ordinal: 0, Role: "user", Content: "archived",
	}}))
	// Reproduce the previous released schema in this isolated fixture.
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "DROP TABLE claude_subagent_sources")
		return err
	}))
	path := database.Path()
	require.NoError(t, database.Close())
	migrated, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, migrated.Close()) })
	paths, err := migrated.GetClaudeSubagentSources(t.Context(), "existing")
	require.NoError(t, err)
	assert.Empty(t, paths)
	messages, err := migrated.GetAllMessages(t.Context(), "existing")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "archived", messages[0].Content)
}
