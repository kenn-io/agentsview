package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

// seedRecentEdit inserts a session and a message with one Edit tool call into
// the database, matching the shape seedEdit uses in the db package tests.
func seedRecentEdit(
	t *testing.T, d *db.DB,
	project, sessionID string,
	ordinal, callIndex int,
	filePath, ts string,
) {
	t.Helper()
	require.NoError(t, d.UpsertSession(db.Session{
		ID:           sessionID,
		Project:      project,
		Machine:      "test",
		Agent:        "claude",
		MessageCount: 1,
	}), "seedRecentEdit: upsert %s", sessionID)
	msg := db.Message{
		SessionID:     sessionID,
		Ordinal:       ordinal,
		Role:          "assistant",
		Content:       "[Edit: " + filePath + "]",
		ContentLength: 10,
		Timestamp:     ts,
		HasToolUse:    true,
		ToolCalls: []db.ToolCall{
			{
				SessionID: sessionID,
				ToolName:  "Edit",
				Category:  "Edit",
				FilePath:  filePath,
				CallIndex: callIndex,
			},
		},
	}
	require.NoError(t, d.InsertMessages([]db.Message{msg}),
		"seedRecentEdit: insert message %s/%d", sessionID, ordinal)
}

// TestRecentEditsReturnsFiles confirms the endpoint returns seeded files in
// the response body with the expected fields populated.
func TestRecentEditsReturnsFiles(t *testing.T) {
	database := dbtest.OpenTestDB(t)

	seedRecentEdit(t, database, "proj", "s1", 1, 0, "main.go", "2026-06-24T10:00:00Z")

	s := newRoutedTestServerWithStore(t, database)
	w := serveGet(t, s, "/api/v1/recent-edits")
	assertRecorderStatus(t, w, http.StatusOK)

	var result db.RecentEditsResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.Len(t, result.Files, 1)
	assert.Equal(t, "proj", result.Files[0].Project)
	assert.Equal(t, "main.go", result.Files[0].FilePath)
	assert.Equal(t, 1, result.Files[0].EditCount)
	assert.False(t, result.HasMore)
}

// TestRecentEditsHasMoreTrue confirms has_more is true when the number of
// file groups exceeds the requested limit.
func TestRecentEditsHasMoreTrue(t *testing.T) {
	database := dbtest.OpenTestDB(t)

	// Three distinct (project, file_path) groups.
	seedRecentEdit(t, database, "proj", "s1", 1, 0, "a.go", "2026-06-24T10:00:00Z")
	seedRecentEdit(t, database, "proj", "s2", 1, 0, "b.go", "2026-06-24T09:00:00Z")
	seedRecentEdit(t, database, "proj", "s3", 1, 0, "c.go", "2026-06-24T08:00:00Z")

	s := newRoutedTestServerWithStore(t, database)
	w := serveGet(t, s, "/api/v1/recent-edits?limit=2")
	assertRecorderStatus(t, w, http.StatusOK)

	var result db.RecentEditsResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	assert.Len(t, result.Files, 2, "limit=2 should return 2 files")
	assert.True(t, result.HasMore, "has_more must be true when a third file exists")
}

// TestRecentEditsProjectFilter confirms the project query param narrows
// results to only that project's files.
func TestRecentEditsProjectFilter(t *testing.T) {
	database := dbtest.OpenTestDB(t)

	seedRecentEdit(t, database, "alpha", "sAlpha", 1, 0, "a.go", "2026-06-24T10:00:00Z")
	seedRecentEdit(t, database, "beta", "sBeta", 1, 0, "b.go", "2026-06-24T10:00:00Z")

	s := newRoutedTestServerWithStore(t, database)
	w := serveGet(t, s, "/api/v1/recent-edits?project=alpha")
	assertRecorderStatus(t, w, http.StatusOK)

	var result db.RecentEditsResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.Len(t, result.Files, 1, "project filter should narrow to alpha only")
	assert.Equal(t, "alpha", result.Files[0].Project)
	assert.Equal(t, "a.go", result.Files[0].FilePath)
}

// TestRecentEditsOffset confirms offset skips the first N file groups.
func TestRecentEditsOffset(t *testing.T) {
	database := dbtest.OpenTestDB(t)

	// Two files; a.go has the newer timestamp so it ranks first.
	seedRecentEdit(t, database, "proj", "s1", 1, 0, "a.go", "2026-06-24T10:00:00Z")
	seedRecentEdit(t, database, "proj", "s2", 1, 0, "b.go", "2026-06-24T09:00:00Z")

	s := newRoutedTestServerWithStore(t, database)
	// offset=1 should skip a.go and return only b.go.
	w := serveGet(t, s, "/api/v1/recent-edits?offset=1")
	assertRecorderStatus(t, w, http.StatusOK)

	var result db.RecentEditsResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.Len(t, result.Files, 1, "offset=1 should skip first file")
	assert.Equal(t, "b.go", result.Files[0].FilePath)
}
