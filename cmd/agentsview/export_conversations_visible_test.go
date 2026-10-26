package main

import (
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestExportConversationsVisibleDialogue(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(testDataDir(t), "sessions.db"))
	insertExportSessionsTestSession(t, database, db.Session{ID: "chat", Project: "sample", Machine: "local", Agent: "sample-agent"})
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{
		{SessionID: "chat", Ordinal: 0, Role: "assistant", SourceUUID: "reply", Content: "[Thinking]\nprivate reasoning\n[/Thinking]\nThe literal [Tool: Read] is an example.", DialogueText: new("The literal [Tool: Read] is an example.")},
		{SessionID: "chat", Ordinal: 1, Role: "assistant", SourceUUID: "old", Content: "Older unclassified content"},
		{SessionID: "chat", Ordinal: 2, Role: "assistant", SourceUUID: "tool-only", Content: "[Tool: Read] arguments", DialogueText: new("")},
	}))
	require.NoError(t, database.Close())
	stdout, _, err := executeExportSessionsCommand(newRootCommand(), "export", "conversations", "changes", "--text-mode", "visible")
	require.NoError(t, err)
	var page struct {
		TextMode   string                  `json:"text_mode"`
		DatabaseID string                  `json:"database_id"`
		Checkpoint string                  `json:"checkpoint"`
		Changes    []db.ConversationChange `json:"changes"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &page))
	assert.Equal(t, "visible", page.TextMode)
	require.Len(t, page.Changes, 3)
	for _, change := range page.Changes {
		stdout, _, err = executeExportSessionsCommand(newRootCommand(), "export", "conversations", "message", "chat", change.MessageID, "--database-id", page.DatabaseID, "--revision", change.Revision, "--text-mode", "visible")
		require.NoError(t, err)
		var body db.ConversationMessage
		require.NoError(t, json.Unmarshal([]byte(stdout), &body))
		switch change.Ordinal {
		case 0:
			require.NotNil(t, body.Text)
			assert.Equal(t, "The literal [Tool: Read] is an example.", *body.Text)
		case 1:
			assert.Equal(t, "visible_text_unavailable", body.Gap)
			assert.Nil(t, body.Text)
		case 2:
			assert.Equal(t, "no_visible_text", body.Gap)
			assert.Nil(t, body.Text)
		}
	}
	stdout, _, err = executeExportSessionsCommand(newRootCommand(), "export", "conversations", "changes", "--checkpoint", page.Checkpoint)
	require.ErrorIs(t, err, db.ErrInvalidCursor)
	assert.Empty(t, stdout)
}
