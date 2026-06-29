//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// seedDuckEdit seeds a session, message, and one Edit tool_call into a local
// SQLite DB using the real write paths. Machine and Agent match syncSession so
// the push includes them.
func seedDuckEdit(
	t *testing.T, local *db.DB,
	project, sessionID string,
	ordinal, callIndex int,
	filePath, ts string,
) {
	t.Helper()
	s := db.Session{
		ID:           sessionID,
		Project:      project,
		Machine:      "local",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}
	require.NoError(t, local.UpsertSession(s), "seedDuckEdit upsertSession %s", sessionID)
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
	require.NoError(t, local.InsertMessages([]db.Message{msg}),
		"seedDuckEdit insertMessages %s/%d", sessionID, ordinal)
}

// seedDuckEditTrashed seeds an edit then marks the session as trashed.
func seedDuckEditTrashed(
	t *testing.T, local *db.DB,
	project, sessionID string,
	ordinal, callIndex int,
	filePath, ts string,
) {
	t.Helper()
	seedDuckEdit(t, local, project, sessionID, ordinal, callIndex, filePath, ts)
	require.NoError(t, local.SoftDeleteSession(sessionID),
		"seedDuckEditTrashed mark deleted %s", sessionID)
}

// newDuckRecentEditsStore seeds the local SQLite DB with the provided setup
// function, pushes to a fresh DuckDB mirror, and returns the read-only Store.
func newDuckRecentEditsStore(
	t *testing.T, setup func(local *db.DB),
) *Store {
	t.Helper()
	ctx := context.Background()
	local := newLocalDB(t)
	setup(local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err, "Push to DuckDB mirror")
	return NewStoreFromDB(syncer.DB())
}

func TestDuckRecentEditsGroupingAndOrdering(t *testing.T) {
	ctx := context.Background()
	store := newDuckRecentEditsStore(t, func(local *db.DB) {
		// projA: edits config.go twice (newer ts), projB: edits config.go once.
		seedDuckEdit(t, local, "projA", "sA1", 1, 0, "config.go", "2026-06-24T10:00:00Z")
		seedDuckEdit(t, local, "projA", "sA1", 2, 0, "config.go", "2026-06-24T12:00:00Z")
		seedDuckEdit(t, local, "projB", "sB1", 1, 0, "config.go", "2026-06-24T11:00:00Z")
	})

	res, err := store.RecentEdits(ctx, db.RecentEditsParams{})
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

func TestDuckRecentEditsExcludesTrash(t *testing.T) {
	ctx := context.Background()
	store := newDuckRecentEditsStore(t, func(local *db.DB) {
		seedDuckEdit(t, local, "proj", "sLive", 1, 0, "main.go", "2026-06-24T10:00:00Z")
		seedDuckEditTrashed(t, local, "proj", "sDead", 1, 0, "deleted.go", "2026-06-24T11:00:00Z")
	})

	res, err := store.RecentEdits(ctx, db.RecentEditsParams{})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "trashed session's edits must be excluded")
	assert.Equal(t, "main.go", res.Files[0].FilePath)
}

func TestDuckRecentEditsProjectFilter(t *testing.T) {
	ctx := context.Background()
	store := newDuckRecentEditsStore(t, func(local *db.DB) {
		seedDuckEdit(t, local, "alpha", "sAlpha", 1, 0, "a.go", "2026-06-24T10:00:00Z")
		seedDuckEdit(t, local, "beta", "sBeta", 1, 0, "b.go", "2026-06-24T10:00:00Z")
	})

	res, err := store.RecentEdits(ctx, db.RecentEditsParams{Project: "alpha"})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "project filter should narrow to alpha only")
	assert.Equal(t, "alpha", res.Files[0].Project)
	assert.Equal(t, "a.go", res.Files[0].FilePath)
}

func TestDuckRecentEditsSearchFilter(t *testing.T) {
	ctx := context.Background()
	store := newDuckRecentEditsStore(t, func(local *db.DB) {
		seedDuckEdit(t, local, "proj", "s1", 1, 0, "internal/db/Recent.go", "2026-06-24T10:00:00Z")
		seedDuckEdit(t, local, "proj", "s2", 1, 0, "internal/server/handler.go", "2026-06-24T09:00:00Z")
		seedDuckEdit(t, local, "proj", "s3", 1, 0, "x/a_b.go", "2026-06-24T08:00:00Z")
		seedDuckEdit(t, local, "proj", "s4", 1, 0, "x/axb.go", "2026-06-24T07:00:00Z")
		seedDuckEdit(t, local, "other", "s5", 1, 0, "internal/db/Recent.go", "2026-06-24T06:00:00Z")
	})

	// Case-insensitive substring over the full path (ILIKE), across projects.
	res, err := store.RecentEdits(ctx, db.RecentEditsParams{Search: "recent"})
	require.NoError(t, err)
	require.Len(t, res.Files, 2, "matches Recent.go in both projects case-insensitively")

	// Matches a directory segment, not just the basename.
	res, err = store.RecentEdits(ctx, db.RecentEditsParams{Search: "server/"})
	require.NoError(t, err)
	require.Len(t, res.Files, 1)
	assert.Equal(t, "internal/server/handler.go", res.Files[0].FilePath)

	// Underscores are literal, not wildcards (db.EscapeLikePattern + ESCAPE).
	res, err = store.RecentEdits(ctx, db.RecentEditsParams{Search: "a_b"})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "escaped underscore matches literally, not as a wildcard")
	assert.Equal(t, "x/a_b.go", res.Files[0].FilePath)

	// Project and search compose.
	res, err = store.RecentEdits(ctx, db.RecentEditsParams{Project: "proj", Search: "recent"})
	require.NoError(t, err)
	require.Len(t, res.Files, 1, "project and search both apply")
	assert.Equal(t, "proj", res.Files[0].Project)
}

func TestDuckRecentEditsTruncationAndHasMore(t *testing.T) {
	ctx := context.Background()
	store := newDuckRecentEditsStore(t, func(local *db.DB) {
		// Three distinct files in same project.
		seedDuckEdit(t, local, "proj", "s1", 1, 0, "a.go", "2026-06-24T10:00:00Z")
		seedDuckEdit(t, local, "proj", "s2", 1, 0, "b.go", "2026-06-24T09:00:00Z")
		seedDuckEdit(t, local, "proj", "s3", 1, 0, "c.go", "2026-06-24T08:00:00Z")
		// a.go gets a second edit so MaxEditsPerFile can be tested.
		seedDuckEdit(t, local, "proj", "s1", 2, 0, "a.go", "2026-06-24T11:00:00Z")
	})

	// Limit=2: HasMore should be true, only 2 files returned.
	res, err := store.RecentEdits(ctx, db.RecentEditsParams{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, res.Files, 2, "only Limit files should be returned")
	assert.True(t, res.HasMore, "HasMore should be true when more files exist")

	// MaxEditsPerFile=1 with a file that has 2 edits => EditsTruncated.
	res2, err := store.RecentEdits(ctx, db.RecentEditsParams{MaxEditsPerFile: 1})
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

func TestDuckRecentEditsNullTimestampsSortLast(t *testing.T) {
	ctx := context.Background()
	store := newDuckRecentEditsStore(t, func(local *db.DB) {
		// File with a real timestamp.
		seedDuckEdit(t, local, "proj", "sReal", 1, 0, "real.go", "2026-06-24T10:00:00Z")
		// File with a null timestamp (empty string maps to NULL in INSERT path).
		seedDuckEdit(t, local, "proj", "sNull", 1, 0, "null.go", "")
	})

	res, err := store.RecentEdits(ctx, db.RecentEditsParams{})
	require.NoError(t, err)
	require.Len(t, res.Files, 2, "both files should appear")
	// real.go (has timestamp) should sort before null.go (NULL timestamp).
	assert.Equal(t, "real.go", res.Files[0].FilePath, "timestamped file first")
	assert.Equal(t, "null.go", res.Files[1].FilePath, "null-timestamp file last")
}

func TestDuckRecentEditsTieByCallIndex(t *testing.T) {
	ctx := context.Background()
	store := newDuckRecentEditsStore(t, func(local *db.DB) {
		s := db.Session{
			ID:           "sTie",
			Project:      "proj",
			Machine:      "local",
			Agent:        "claude",
			MessageCount: 1,
			CreatedAt:    "2026-01-01T00:00:00Z",
		}
		require.NoError(t, local.UpsertSession(s))
		// Same session, same ordinal, two Edit calls: callIndex 0 and 1.
		// Higher call_index (1) should rank first (rn=1).
		msg := db.Message{
			SessionID:     "sTie",
			Ordinal:       1,
			Role:          "assistant",
			Content:       "[Edit: tie.go x2]",
			ContentLength: 17,
			Timestamp:     "2026-06-24T10:00:00Z",
			HasToolUse:    true,
			ToolCalls: []db.ToolCall{
				{SessionID: "sTie", ToolName: "Edit", Category: "Edit",
					FilePath: "tie.go", CallIndex: 0},
				{SessionID: "sTie", ToolName: "Edit", Category: "Edit",
					FilePath: "tie.go", CallIndex: 1},
			},
		}
		require.NoError(t, local.InsertMessages([]db.Message{msg}))
	})

	res, err := store.RecentEdits(ctx, db.RecentEditsParams{MaxEditsPerFile: 5})
	require.NoError(t, err)
	require.Len(t, res.Files, 1)
	require.Len(t, res.Files[0].Edits, 2, "both edits inlined")
	// rn=1 is the first in the Edits slice; it must be the higher call_index.
	assert.Equal(t, 1, res.Files[0].Edits[0].CallIndex, "higher call_index ranks first")
	assert.Equal(t, 0, res.Files[0].Edits[1].CallIndex)
}
