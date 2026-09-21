package main

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestExportConversationSchemaIsRequiredOnlyForConversations(t *testing.T) {
	for name, setup := range map[string]string{
		"missing tables":            `DROP TABLE conversation_messages; DROP TABLE conversation_session_changes`,
		"incomplete initialization": `DELETE FROM archive_metadata WHERE key = 'conversation_export_initialized'`,
	} {
		t.Run(name, func(t *testing.T) {
			database := seedExportSessionsArchive(t)
			path := database.Path()
			require.NoError(t, database.Close())
			raw, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			_, err = raw.ExecContext(t.Context(), setup)
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			stdout, stderr, err := executeExportSessionsCommand(newRootCommand(), "export", "sessions", "--format", "json")
			require.NoError(t, err)
			assert.Empty(t, stderr)
			document := decodeExportSessionsDocument(t, stdout)
			require.Len(t, document.Sessions, 2)
			assert.Equal(t, "alpha-new", document.Sessions[0].ID)
			assert.Equal(t, "alpha-old", document.Sessions[1].ID)
			for _, args := range [][]string{
				{"export", "conversations", "changes"},
				{"export", "conversations", "message", "alpha-new", "message-a", "--revision", "1", "--database-id", "export-sessions-test-db"},
			} {
				stdout, _, err := executeExportSessionsCommand(newRootCommand(), args...)
				require.Error(t, err)
				if name == "incomplete initialization" {
					require.ErrorIs(t, err, db.ErrConversationInitializationRequired)
				} else {
					assert.True(t, db.IsSchemaUpgradeRequired(err))
				}
				assert.Contains(t, err.Error(), "agentsview daemon restart")
				assert.Empty(t, stdout)
			}
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after, "read-only export must not run archive migrations")
		})
	}
}

func TestExportConversationsEmptyCheckpoint(t *testing.T) {
	seedExportReportingArchive(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	stdout, stderr, err := executeExportSessionsCommand(
		newExportReportingTestRoot(now), "export", "conversations", "changes",
	)
	require.NoError(t, err)
	assert.Empty(t, stderr)
	var first struct {
		SchemaVersion int              `json:"schema_version"`
		Checkpoint    string           `json:"checkpoint"`
		Changes       []jsontext.Value `json:"changes"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &first))
	assert.Equal(t, 1, first.SchemaVersion)
	require.NotEmpty(t, first.Checkpoint)
	assert.Empty(t, first.Changes)
	stdout, stderr, err = executeExportSessionsCommand(
		newExportReportingTestRoot(now), "export", "conversations", "changes",
		"--checkpoint", first.Checkpoint,
	)
	require.NoError(t, err)
	assert.Empty(t, stderr)
	var next struct {
		Checkpoint string           `json:"checkpoint"`
		Changes    []jsontext.Value `json:"changes"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &next))
	assert.Equal(t, first.Checkpoint, next.Checkpoint)
	assert.Empty(t, next.Changes)
}

func TestExportConversationUsagePolicyRejectsStoredBodyBeforeRestart(t *testing.T) {
	path := filepath.Join(testDataDir(t), "sessions.db")
	t.Setenv("AGENTSVIEW_ARCHIVE_CONTENT", "full")
	database := dbtest.OpenTestDBAt(t, path)
	insertExportSessionsTestSession(t, database, db.Session{
		ID: "chat", Project: "sample", Machine: "local", Agent: "gemini",
	})
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{
		SessionID: "chat", Role: "assistant", Content: "Saved reply", SourceUUID: "reply-one",
	}}))
	page, err := database.ExportConversationChanges(t.Context(), db.ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, page.Changes, 1)
	change := page.Changes[0]
	require.NoError(t, database.Close())
	args := []string{
		"export", "conversations", "message", change.SessionID, change.MessageID,
		"--revision", change.Revision, "--database-id", page.DatabaseID,
	}
	stdout, _, err := executeExportSessionsCommand(newRootCommand(), args...)
	require.NoError(t, err)
	assert.Contains(t, stdout, "Saved reply")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	t.Setenv("AGENTSVIEW_ARCHIVE_CONTENT", "usage")
	stdout, _, err = executeExportSessionsCommand(newRootCommand(), args...)
	require.ErrorIs(t, err, db.ErrArchiveContentExcluded)
	assert.Empty(t, stdout)
	stdout, _, err = executeExportSessionsCommand(newRootCommand(), "export", "conversations", "changes")
	require.NoError(t, err, "text-free change listings remain available")
	assert.NotContains(t, stdout, "Saved reply")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "read-only exports must not rewrite the archive")
}

func TestExportConversationsPagesBodiesAndCorrections(t *testing.T) {
	path := filepath.Join(testDataDir(t), "sessions.db")
	database := dbtest.OpenTestDBAt(t, path)
	insertExportSessionsTestSession(t, database, db.Session{
		ID: "conversation-a", Project: "project-a", Machine: "local", Agent: "claude",
		MessageCount: 2, UserMessageCount: 2,
	})
	rows := []db.Message{
		{SessionID: "conversation-a", Ordinal: 0, Role: "user", Content: "αβγ", SourceUUID: "native-a"},
		{SessionID: "conversation-a", Ordinal: 1, Role: "user", Content: "αβγ", SourceUUID: "native-b"},
	}
	require.NoError(t, database.InsertMessages(t.Context(), rows))

	stdout, stderr, err := executeExportSessionsCommand(newRootCommand(),
		"export", "conversations", "changes", "--limit", "1")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.NotContains(t, stdout, "αβγ", "the change listing must not contain message text")
	var page db.ConversationExportResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &page))
	require.Len(t, page.Changes, 1)
	require.NotEmpty(t, page.NextCursor)
	assert.Empty(t, page.Checkpoint)
	var messages []db.ConversationChange
	for {
		for _, change := range page.Changes {
			if change.Type == "message" {
				messages = append(messages, change)
			}
		}
		if page.NextCursor == "" {
			break
		}
		stdout, stderr, err = executeExportSessionsCommand(newRootCommand(),
			"export", "conversations", "changes", "--cursor", page.NextCursor, "--limit", "1")
		require.NoError(t, err)
		assert.Empty(t, stderr)
		require.NoError(t, json.Unmarshal([]byte(stdout), &page))
	}
	require.Len(t, messages, 2)
	first := messages[0]
	assert.NotEqual(t, first.MessageID, messages[1].MessageID, "identical utterances remain distinct")
	require.NotEmpty(t, page.Checkpoint)
	checkpoint := page.Checkpoint

	stdout, stderr, err = executeExportSessionsCommand(newRootCommand(),
		"export", "conversations", "message", first.SessionID, first.MessageID,
		"--revision", first.Revision, "--database-id", page.DatabaseID, "--max-bytes", "4")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	var body db.ConversationMessage
	require.NoError(t, json.Unmarshal([]byte(stdout), &body))
	require.NotNil(t, body.Text)
	assert.Equal(t, "αβ", *body.Text)
	assert.EqualValues(t, 4, body.NextOffset)
	assert.EqualValues(t, 6, body.TextBytes)
	stdout, _, err = executeExportSessionsCommand(newRootCommand(),
		"export", "conversations", "message", first.SessionID, first.MessageID,
		"--revision", first.Revision, "--database-id", page.DatabaseID, "--offset", "4", "--max-bytes", "4")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(stdout), &body))
	require.NotNil(t, body.Text)
	assert.Equal(t, "γ", *body.Text)
	assert.EqualValues(t, 6, body.NextOffset)

	rows[0].Content = "corrected"
	require.NoError(t, database.ReplaceSessionMessages(t.Context(), "conversation-a", rows))
	stdout, stderr, err = executeExportSessionsCommand(newRootCommand(),
		"export", "conversations", "message", first.SessionID, first.MessageID,
		"--revision", first.Revision, "--database-id", page.DatabaseID)
	require.ErrorIs(t, err, db.ErrConversationRevisionChanged)
	assert.Equal(t, 5, exitCodeFromError(err))
	assert.Contains(t, stderr, `"error":"revision_changed"`)
	assert.Empty(t, stdout, "stale reads must not return newer text under an old revision")
	stdout, _, err = executeExportSessionsCommand(newRootCommand(),
		"export", "conversations", "changes", "--checkpoint", checkpoint)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(stdout), &page))
	require.Len(t, page.Changes, 1)
	assert.Equal(t, first.MessageID, page.Changes[0].MessageID)
	assert.NotEqual(t, first.Revision, page.Changes[0].Revision)
	require.NoError(t, database.ReplaceSessionMessages(t.Context(), "conversation-a", rows[1:]))
	stdout, stderr, err = executeExportSessionsCommand(newRootCommand(),
		"export", "conversations", "message", first.SessionID, first.MessageID,
		"--revision", page.Changes[0].Revision, "--database-id", page.DatabaseID)
	require.ErrorIs(t, err, db.ErrConversationRevisionChanged)
	assert.Equal(t, 5, exitCodeFromError(err))
	assert.Contains(t, stderr, `"error":"revision_changed"`)
	assert.Empty(t, stdout)

	// A rebuilt generation must not accept the saved checkpoint or a
	// revision-pinned body request from the previous database.
	previousDatabaseID := page.DatabaseID
	require.NoError(t, database.SetDatabaseIDForTest(t.Context(), "replacement-generation"))
	stdout, stderr, err = executeExportSessionsCommand(newRootCommand(),
		"export", "conversations", "changes", "--checkpoint", page.Checkpoint)
	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, `"error":"reconciliation_required"`)
	stdout, stderr, err = executeExportSessionsCommand(newRootCommand(),
		"export", "conversations", "message", first.SessionID, first.MessageID,
		"--revision", page.Changes[0].Revision, "--database-id", previousDatabaseID)
	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, `"error":"reconciliation_required"`)
}
