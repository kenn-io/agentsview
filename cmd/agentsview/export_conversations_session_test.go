package main

import (
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestExportConversationSessionWritesJSONLines(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(testDataDir(t), "sessions.db"))
	insertExportSessionsTestSession(t, database, db.Session{
		ID: "chat", Project: "sample", Machine: "local", Agent: "claude", MessageCount: 2, UserMessageCount: 1,
	})
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{
		{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Check the build.", SourceUUID: "ask"},
		{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "It passes.", SourceUUID: "reply"},
	}))
	require.NoError(t, database.Close())

	// The archive is cold: the command builds its index before reading.
	stdout, stderr, err := executeExportSessionsCommand(newRootCommand(), "export", "conversations", "session", "chat", "--max-bytes", "4")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	var types, text []string
	for _, line := range lines {
		var record struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &record), "each line is one JSON record: %s", line)
		types = append(types, record.Type)
		text = append(text, record.Text)
	}
	assert.Equal(t, []string{"header", "message", "text", "text", "text", "text", "message", "text", "text", "text", "trailer"}, types)
	assert.Equal(t, "Check the build.", strings.Join(text[2:6], ""))
	assert.Equal(t, "It passes.", strings.Join(text[7:10], ""))
	assert.JSONEq(t, `{"type":"trailer","messages":2}`, lines[len(lines)-1])
}

func TestExportConversationSessionReportsUnknownSession(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(testDataDir(t), "sessions.db"))
	require.NoError(t, database.Close())

	stdout, stderr, err := executeExportSessionsCommand(newRootCommand(), "export", "conversations", "session", "missing")
	require.Error(t, err)
	assert.Equal(t, conversationSessionNotFoundExitCode, exitCodeFromError(err))
	assert.Empty(t, stdout)
	assert.JSONEq(t, `{"error":"session_not_found","message":"the archive has no record of this session"}`, stderr)
}
