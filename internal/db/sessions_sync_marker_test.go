//go:build fts5

package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncMarkerMaintainedByTriggers(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	sess := Session{ID: "sm-1", Project: "p", Machine: "m", Agent: "claude-code",
		CreatedAt: "2026-07-01T10:00:00.000Z"}
	require.NoError(t, database.UpsertSession(sess))

	// UpsertSession does not write created_at (it relies on the schema
	// DEFAULT for new rows), so backdate it directly the way
	// TestListSessionsModifiedBetween does, which also exercises the
	// AFTER UPDATE OF created_at trigger.
	_, err := database.getWriter().ExecContext(ctx,
		`UPDATE sessions SET created_at = ? WHERE id = ?`, sess.CreatedAt, "sm-1")
	require.NoError(t, err)

	var marker string
	require.NoError(t, database.getReader().QueryRowContext(ctx,
		`SELECT sync_marker FROM sessions WHERE id = ?`, "sm-1").Scan(&marker))
	assert.Equal(t, "2026-07-01T10:00:00.000Z", marker)

	// Bumping a later signal advances the marker.
	_, err = database.getWriter().ExecContext(ctx,
		`UPDATE sessions SET ended_at = '2026-07-02T09:30:00.000Z' WHERE id = ?`, "sm-1")
	require.NoError(t, err)
	require.NoError(t, database.getReader().QueryRowContext(ctx,
		`SELECT sync_marker FROM sessions WHERE id = ?`, "sm-1").Scan(&marker))
	assert.Equal(t, "2026-07-02T09:30:00.000Z", marker)

	// file_mtime (ns) participates and wins when newest.
	_, err = database.getWriter().ExecContext(ctx,
		`UPDATE sessions SET file_mtime = 1783069200500000000 WHERE id = ?`, "sm-1")
	require.NoError(t, err)
	require.NoError(t, database.getReader().QueryRowContext(ctx,
		`SELECT sync_marker FROM sessions WHERE id = ?`, "sm-1").Scan(&marker))
	assert.Equal(t, "2026-07-03T09:00:00.500Z", marker)
}

func TestListSessionsForMirrorWindowInclusiveBounds(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	sessions := []Session{
		{ID: "w-1", Project: "p", Machine: "m", Agent: "a", CreatedAt: "2026-07-01T10:00:00.000Z"},
		{ID: "w-2", Project: "p", Machine: "m", Agent: "a", CreatedAt: "2026-07-01T10:00:00.001Z"},
		{ID: "w-3", Project: "p", Machine: "m", Agent: "a", CreatedAt: "2026-07-01T09:59:59.999Z"},
	}
	for _, s := range sessions {
		require.NoError(t, database.UpsertSession(s))
	}
	// UpsertSession relies on the schema DEFAULT for created_at on new rows,
	// so backdate it directly (same pattern as TestListSessionsModifiedBetween)
	// to get deterministic sync_marker values via the AFTER UPDATE OF trigger.
	for _, s := range sessions {
		_, err := database.getWriter().ExecContext(ctx,
			`UPDATE sessions SET created_at = ? WHERE id = ?`, s.CreatedAt, s.ID)
		require.NoError(t, err)
	}

	// Inclusive lower bound: a session whose marker EQUALS since must be selected.
	got, err := database.ListSessionsForMirrorWindow(ctx,
		"2026-07-01T10:00:00.000Z", "2026-07-01T10:00:00.001Z", nil, nil)
	require.NoError(t, err)
	ids := make([]string, 0, len(got))
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	assert.ElementsMatch(t, []string{"w-1", "w-2"}, ids)

	// Empty bounds list everything; project filters apply.
	all, err := database.ListSessionsForMirrorWindow(ctx, "", "", nil, nil)
	require.NoError(t, err)
	assert.Len(t, all, 3)
	none, err := database.ListSessionsForMirrorWindow(ctx, "", "", []string{"other"}, nil)
	require.NoError(t, err)
	assert.Empty(t, none)
}
