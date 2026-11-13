package main

import (
	"encoding/json/v2"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

// exportFakeAWSKey is an obviously fake AWS access key ID, split so this file
// does not hold the whole token.
const exportFakeAWSKey = "AKIA" + "7QHWN2DKR4FYPLJM"

func TestExportConversationMessageMasksSecretsUnlessRevealed(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(testDataDir(t), "sessions.db"))
	insertExportSessionsTestSession(t, database, db.Session{
		ID: "leaky", Project: "sample", Machine: "local", Agent: "claude",
		MessageCount: 1, UserMessageCount: 1,
	})
	stored := "use key " + exportFakeAWSKey + " for the deploy"
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{
		{SessionID: "leaky", Ordinal: 0, Role: "user", Content: stored, SourceUUID: "native-leaky"},
	}))
	require.NoError(t, database.Close())

	stdout, _, err := executeExportSessionsCommand(newRootCommand(), "export", "conversations", "changes")
	require.NoError(t, err)
	var page db.ConversationExportResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &page))
	require.Len(t, page.Changes, 1)
	change := page.Changes[0]
	require.EqualValues(t, len(stored), change.TextBytes)

	readMessage := func(t *testing.T, offset int64, maxBytes int, extra ...string) db.ConversationMessage {
		t.Helper()
		args := append([]string{
			"export", "conversations", "message", change.SessionID, change.MessageID,
			"--revision", change.Revision, "--database-id", page.DatabaseID,
			"--offset", strconv.FormatInt(offset, 10), "--max-bytes", strconv.Itoa(maxBytes),
		}, extra...)
		stdout, stderr, err := executeExportSessionsCommand(newRootCommand(), args...)
		require.NoError(t, err)
		require.Empty(t, stderr)
		assert.NotContains(t, stdout, exportFakeAWSKey[4:], "no chunk may carry the unmasked key body")
		var body db.ConversationMessage
		require.NoError(t, json.Unmarshal([]byte(stdout), &body))
		require.NotNil(t, body.Text)
		return body
	}

	t.Run("default masks the whole message", func(t *testing.T) {
		body := readMessage(t, 0, 64*1024)
		want := "use key AKIA…PLJM for the deploy"
		assert.Equal(t, want, *body.Text)
		assert.True(t, body.Redacted)
		assert.EqualValues(t, len(want), body.TextBytes)
		assert.EqualValues(t, len(want), body.NextOffset)
		assert.Equal(t, change.Digest, body.Digest, "the digest still identifies the stored message")
	})

	t.Run("small chunks walk the masked text", func(t *testing.T) {
		// Five-byte chunks split the key across several reads; each read
		// masks the whole message first, so no read sees a partial key.
		var text strings.Builder
		var offset int64
		for {
			body := readMessage(t, offset, 5)
			text.WriteString(*body.Text)
			offset = body.NextOffset
			if offset >= body.TextBytes {
				break
			}
		}
		assert.Equal(t, "use key AKIA…PLJM for the deploy", text.String())
	})

	t.Run("reveal returns stored text", func(t *testing.T) {
		args := []string{
			"export", "conversations", "message", change.SessionID, change.MessageID,
			"--revision", change.Revision, "--database-id", page.DatabaseID, "--reveal",
		}
		stdout, stderr, err := executeExportSessionsCommand(newRootCommand(), args...)
		require.NoError(t, err)
		require.Empty(t, stderr)
		var body db.ConversationMessage
		require.NoError(t, json.Unmarshal([]byte(stdout), &body))
		require.NotNil(t, body.Text)
		assert.Equal(t, stored, *body.Text)
		assert.False(t, body.Redacted)
		assert.Equal(t, change.TextBytes, body.TextBytes)
		assert.NotContains(t, stdout, `"redacted"`)
	})
}
