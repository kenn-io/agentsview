package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedEdit inserts a session (project, deleted_at NULL), a message (ordinal,
// timestamp), and one Edit tool_call (filePath, callIndex) using the real
// insert paths.
func seedEdit(
	t *testing.T, d *DB,
	project, sessionID string,
	ordinal, callIndex int,
	filePath, ts string,
) {
	t.Helper()
	// UpsertSession is idempotent; call for every seedEdit so the same
	// sessionID can accumulate multiple messages across calls.
	s := Session{
		ID:           sessionID,
		Project:      project,
		Machine:      defaultMachine,
		Agent:        defaultAgent,
		MessageCount: 1,
	}
	require.NoError(t, d.UpsertSession(s), "seedEdit upsertSession %s", sessionID)

	msg := Message{
		SessionID:     sessionID,
		Ordinal:       ordinal,
		Role:          "assistant",
		Content:       "[Edit: " + filePath + "]",
		ContentLength: 10,
		Timestamp:     ts,
		HasToolUse:    true,
		ToolCalls: []ToolCall{
			{
				SessionID: sessionID,
				ToolName:  "Edit",
				Category:  "Edit",
				FilePath:  filePath,
				CallIndex: callIndex,
			},
		},
	}
	require.NoError(t, d.InsertMessages([]Message{msg}), "seedEdit insertMessages %s/%d", sessionID, ordinal)
}

// seedEditTrashed is like seedEdit but marks the session as deleted.
func seedEditTrashed(
	t *testing.T, d *DB,
	project, sessionID string,
	ordinal, callIndex int,
	filePath, ts string,
) {
	t.Helper()
	seedEdit(t, d, project, sessionID, ordinal, callIndex, filePath, ts)
	_, err := d.getWriter().Exec(
		`UPDATE sessions SET deleted_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), sessionID,
	)
	require.NoError(t, err, "seedEditTrashed mark deleted %s", sessionID)
}

func TestRecentEditsGroupingAndOrdering(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// projA: edits config.go twice (newer ts), projB: edits config.go once.
	seedEdit(t, d, "projA", "sA1", 1, 0, "config.go", "2026-06-24T10:00:00Z")
	seedEdit(t, d, "projA", "sA1", 2, 0, "config.go", "2026-06-24T12:00:00Z")
	seedEdit(t, d, "projB", "sB1", 1, 0, "config.go", "2026-06-24T11:00:00Z")

	res, err := d.RecentEdits(ctx, RecentEditsParams{})
	require.NoError(t, err)
	require.Len(t, res.Files, 2, "same relative path in two projects = 2 rows")
	// Most recent group first: projA (12:00) before projB (11:00).
	assert.Equal(t, "projA", res.Files[0].Project)
	assert.Equal(t, "config.go", res.Files[0].FilePath)
	assert.Equal(t, 2, res.Files[0].EditCount)
	assert.Equal(t, "projB", res.Files[1].Project)
	assert.Equal(t, "config.go", res.Files[1].FilePath)
	assert.Equal(t, 1, res.Files[1].EditCount)
	assert.False(t, res.HasMore)
}

func TestRecentEditsExcludesTrash(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedEdit(t, d, "proj", "sLive", 1, 0, "main.go", "2026-06-24T10:00:00Z")
	seedEditTrashed(t, d, "proj", "sDead", 1, 0, "deleted.go", "2026-06-24T11:00:00Z")

	res, err := d.RecentEdits(ctx, RecentEditsParams{})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "trashed session's edits must be excluded")
	assert.Equal(t, "main.go", res.Files[0].FilePath)
}

func TestRecentEditsProjectFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedEdit(t, d, "alpha", "sAlpha", 1, 0, "a.go", "2026-06-24T10:00:00Z")
	seedEdit(t, d, "beta", "sBeta", 1, 0, "b.go", "2026-06-24T10:00:00Z")

	res, err := d.RecentEdits(ctx, RecentEditsParams{Project: "alpha"})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "project filter should narrow to alpha only")
	assert.Equal(t, "alpha", res.Files[0].Project)
	assert.Equal(t, "a.go", res.Files[0].FilePath)
}

func TestRecentEditsSearchFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	seedEdit(t, d, "proj", "s1", 1, 0, "internal/db/Recent.go", "2026-06-24T10:00:00Z")
	seedEdit(t, d, "proj", "s2", 1, 0, "internal/server/handler.go", "2026-06-24T09:00:00Z")

	// Case-insensitive substring over the full path (matches "Recent").
	res, err := d.RecentEdits(ctx, RecentEditsParams{Search: "recent"})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "search matches path substring case-insensitively")
	assert.Equal(t, "internal/db/Recent.go", res.Files[0].FilePath)

	// Matches a directory segment, not just the basename.
	res, err = d.RecentEdits(ctx, RecentEditsParams{Search: "server/"})
	require.NoError(t, err)
	require.Len(t, res.Files, 1)
	assert.Equal(t, "internal/server/handler.go", res.Files[0].FilePath)

	// Underscores are literal, not LIKE wildcards (EscapeLikePattern): "a_b"
	// must match only a_b.go, never axb.go.
	seedEdit(t, d, "proj", "s3", 1, 0, "x/a_b.go", "2026-06-24T08:00:00Z")
	seedEdit(t, d, "proj", "s4", 1, 0, "x/axb.go", "2026-06-24T07:00:00Z")
	res, err = d.RecentEdits(ctx, RecentEditsParams{Search: "a_b"})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "escaped underscore matches literally, not as a wildcard")
	assert.Equal(t, "x/a_b.go", res.Files[0].FilePath)

	// Project and search compose.
	seedEdit(t, d, "other", "s5", 1, 0, "internal/db/Recent.go", "2026-06-24T06:00:00Z")
	res, err = d.RecentEdits(ctx, RecentEditsParams{Project: "proj", Search: "recent"})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "project and search both apply")
	assert.Equal(t, "proj", res.Files[0].Project)
}

func TestRecentEditsTruncationAndHasMore(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Three distinct files in same project.
	seedEdit(t, d, "proj", "s1", 1, 0, "a.go", "2026-06-24T10:00:00Z")
	seedEdit(t, d, "proj", "s2", 1, 0, "b.go", "2026-06-24T09:00:00Z")
	seedEdit(t, d, "proj", "s3", 1, 0, "c.go", "2026-06-24T08:00:00Z")

	// Limit=2: HasMore should be true, only 2 files returned.
	res, err := d.RecentEdits(ctx, RecentEditsParams{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, res.Files, 2, "only Limit files should be returned")
	assert.True(t, res.HasMore, "HasMore should be true when more files exist")

	// MaxEditsPerFile=1 with a file that has 2 edits => EditsTruncated.
	seedEdit(t, d, "proj", "s1", 2, 0, "a.go", "2026-06-24T11:00:00Z")
	res2, err := d.RecentEdits(ctx, RecentEditsParams{MaxEditsPerFile: 1})
	require.NoError(t, err)
	var found bool
	for _, f := range res2.Files {
		if f.FilePath == "a.go" {
			assert.True(t, f.EditsTruncated, "a.go has 2 edits, MaxEditsPerFile=1 => truncated")
			assert.Len(t, f.Edits, 1, "only 1 edit inlined")
			found = true
		}
	}
	assert.True(t, found, "a.go should appear in result")
}

func TestRecentEditsNullTimestampsSortLast(t *testing.T) {

	d := testDB(t)
	ctx := context.Background()

	// File with a real timestamp.
	seedEdit(t, d, "proj", "sReal", 1, 0, "real.go", "2026-06-24T10:00:00Z")
	// File with a null timestamp (empty string maps to NULL in INSERT path).
	seedEdit(t, d, "proj", "sNull", 1, 0, "null.go", "")

	res, err := d.RecentEdits(ctx, RecentEditsParams{})
	require.NoError(t, err)
	require.Len(t, res.Files, 2, "both files should appear")
	// real.go (has timestamp) should sort before null.go (NULL timestamp).
	assert.Equal(t, "real.go", res.Files[0].FilePath, "timestamped file first")
	assert.Equal(t, "null.go", res.Files[1].FilePath, "null-timestamp file last")
}

func TestRecentEditsTieByCallIndex(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Same session, same ordinal, two Edit calls: callIndex 1 and 0.
	// Higher call_index (1) should rank first (rn=1).
	insertSession(t, d, "sTie", "proj")
	msg := Message{
		SessionID:     "sTie",
		Ordinal:       1,
		Role:          "assistant",
		Content:       "[Edit: tie.go x2]",
		ContentLength: 17,
		Timestamp:     "2026-06-24T10:00:00Z",
		HasToolUse:    true,
		ToolCalls: []ToolCall{
			{SessionID: "sTie", ToolName: "Edit", Category: "Edit",
				FilePath: "tie.go", CallIndex: 0},
			{SessionID: "sTie", ToolName: "Edit", Category: "Edit",
				FilePath: "tie.go", CallIndex: 1},
		},
	}
	require.NoError(t, d.InsertMessages([]Message{msg}), "insert tie message")

	res, err := d.RecentEdits(ctx, RecentEditsParams{MaxEditsPerFile: 5})
	require.NoError(t, err)
	require.Len(t, res.Files, 1)
	require.Len(t, res.Files[0].Edits, 2, "both edits inlined")
	// rn=1 is the first in the Edits slice; it must be the higher call_index.
	assert.Equal(t, 1, res.Files[0].Edits[0].CallIndex, "higher call_index ranks first")
	assert.Equal(t, 0, res.Files[0].Edits[1].CallIndex)
}

func TestRecentEditsFileGroupTieByCallIndex(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// One message edits two files at the same timestamp/ordinal. The later
	// tool call (call_index 1) must rank its file ahead, even though its path
	// sorts lexically lower. Guards the file_page last_call_index tie-breaker.
	insertSession(t, d, "sGroup", "proj")
	msg := Message{
		SessionID:     "sGroup",
		Ordinal:       1,
		Role:          "assistant",
		Content:       "[Edit: zzz.go then aaa.go]",
		ContentLength: 26,
		Timestamp:     "2026-06-24T10:00:00Z",
		HasToolUse:    true,
		ToolCalls: []ToolCall{
			{SessionID: "sGroup", ToolName: "Edit", Category: "Edit",
				FilePath: "zzz.go"},
			{SessionID: "sGroup", ToolName: "Edit", Category: "Edit",
				FilePath: "aaa.go"},
		},
	}
	require.NoError(t, d.InsertMessages([]Message{msg}), "insert group message")

	res, err := d.RecentEdits(ctx, RecentEditsParams{})
	require.NoError(t, err)
	require.Len(t, res.Files, 2)
	assert.Equal(t, "aaa.go", res.Files[0].FilePath, "later call_index file first")
	assert.Equal(t, "zzz.go", res.Files[1].FilePath)
}
