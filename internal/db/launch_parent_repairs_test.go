package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatchSpawnEvidenceAvoidsMutualParents(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "A", "project")
	insertSession(t, d, "B", "project")
	_, err := d.SetSessionExternalParent(t.Context(), "A", "B")
	require.NoError(t, err)
	a, err := d.GetSession(t.Context(), "A")
	require.NoError(t, err)
	require.NotNil(t, a.ParentSessionID)
	write := messageCountWrite("A", 1)
	write.Messages[0].Role = "assistant"
	write.Messages[0].ToolCalls = []ToolCall{{ToolName: "Agent", Category: "Task", SubagentSessionID: "B"}}
	_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	a, err = d.GetSession(t.Context(), "A")
	require.NoError(t, err)
	b, err := d.GetSession(t.Context(), "B")
	require.NoError(t, err)
	assert.Nil(t, a.ParentSessionID)
	require.NotNil(t, b.ParentSessionID)
	assert.Equal(t, "A", *b.ParentSessionID)
}

func repairTestLauncher(t *testing.T, d *DB) {
	t.Helper()
	insertSession(t, d, "P", "project", func(s *Session) { s.Agent = "claude"; s.Cwd = "/repo" })
	insertMessages(t, d, Message{SessionID: "P", Ordinal: 0, Role: "assistant", Timestamp: "2026-01-02T03:04:00Z", ToolCalls: []ToolCall{{SessionID: "P", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"codex exec hi"}`, WorkingDirectory: "/repo"}}})
}

func repairTestParent(t *testing.T, d *DB, id string) string {
	t.Helper()
	l, err := d.GetSessionExternalParent(t.Context(), id)
	if err != nil {
		require.ErrorIs(t, err, sql.ErrNoRows)
		return ""
	}
	return l.ParentSessionID
}

func TestStagedMetadataOnlyReparseRelinks(t *testing.T) {
	d := testDB(t)
	repairTestLauncher(t, d)
	started := "2026-01-02T03:04:03Z"
	mk := func(cwd string) SessionBatchWrite {
		return SessionBatchWrite{
			Session:         Session{ID: "R", Project: "project", Machine: "local", Agent: "codex", Cwd: cwd, StartedAt: &started, SessionKind: "non-interactive", MessageCount: 1, UserMessageCount: 1},
			Messages:        []Message{{SessionID: "R", Ordinal: 0, Role: "user", Content: "hi", ContentLength: 2, Timestamp: started}},
			ReplaceMessages: true,
		}
	}
	require.NoError(t, d.WriteSessionBatchStaged(t.Context(), mk("/repo"), newScratchStagedResults(t), nil, nil))
	require.Equal(t, "P", repairTestParent(t, d, "R"), "precondition: linked")
	require.NoError(t, d.WriteSessionBatchStaged(t.Context(), mk("/other"), newScratchStagedResults(t), nil, nil))
	got, err := d.GetSession(t.Context(), "R")
	require.NoError(t, err)
	require.Equal(t, "/other", got.Cwd)
	assert.Empty(t, repairTestParent(t, d, "R"), "run moved to /other but stays under P")
}
