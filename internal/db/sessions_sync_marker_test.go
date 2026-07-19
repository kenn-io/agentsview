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

func TestListSessionsForMirrorWindowInclusiveLowerBoundUnboundedAbove(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	sessions := []Session{
		{ID: "w-1", Project: "p", Machine: "m", Agent: "a", CreatedAt: "2026-07-01T10:00:00.000Z"},
		{ID: "w-2", Project: "p", Machine: "m", Agent: "a", CreatedAt: "2026-07-01T10:00:00.001Z"},
		{ID: "w-3", Project: "p", Machine: "m", Agent: "a", CreatedAt: "2026-07-01T09:59:59.999Z"},
		// w-future simulates a clock-skewed signal: its marker sits far past
		// any realistic wall-clock cutoff and must still be selected, since
		// the window is [since, +inf) with no upper bound.
		{ID: "w-future", Project: "p", Machine: "m", Agent: "a", CreatedAt: "2099-01-01T00:00:00.000Z"},
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

	// Inclusive lower bound: a session whose marker EQUALS since must be
	// selected, and there is no upper bound to exclude the far-future marker.
	got, err := database.ListSessionsForMirrorWindow(ctx,
		"2026-07-01T10:00:00.000Z", nil, nil)
	require.NoError(t, err)
	ids := make([]string, 0, len(got))
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	assert.ElementsMatch(t, []string{"w-1", "w-2", "w-future"}, ids)

	// An empty since lists everything; project filters apply.
	all, err := database.ListSessionsForMirrorWindow(ctx, "", nil, nil)
	require.NoError(t, err)
	assert.Len(t, all, 4)
	none, err := database.ListSessionsForMirrorWindow(ctx, "", []string{"other"}, nil)
	require.NoError(t, err)
	assert.Empty(t, none)
}

// TestSyncMarkerMalformedCreatedAtIsDropped pins the no-raw-fallback
// contract (see syncMarkerSchemaSQL): a malformed created_at must never
// participate in the marker MAX. Letters sort above digits, so a raw
// fallback would let "garbage" beat every normalized "2026-..." timestamp,
// poison the session's marker, and permanently advance the push cutoff
// past all future real changes.
func TestSyncMarkerMalformedCreatedAtIsDropped(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	readMarker := func(id string) string {
		t.Helper()
		var marker string
		require.NoError(t, database.getReader().QueryRowContext(ctx,
			`SELECT sync_marker FROM sessions WHERE id = ?`, id).Scan(&marker))
		return marker
	}

	// Malformed created_at with no other signal: the marker is empty, so
	// the session is invisible to incremental windows (a full rebuild
	// still covers it), matching the PG push's window semantics.
	require.NoError(t, database.UpsertSession(
		Session{ID: "sm-malformed", Project: "p", Machine: "m", Agent: "claude-code"}))
	_, err := database.getWriter().ExecContext(ctx,
		`UPDATE sessions SET created_at = ? WHERE id = ?`, "garbage", "sm-malformed")
	require.NoError(t, err)
	assert.Empty(t, readMarker("sm-malformed"),
		"a malformed created_at must not become the marker via a raw fallback")

	// Malformed created_at plus a valid ended_at: the marker equals the
	// normalized ended_at instead of the lexically larger raw string.
	require.NoError(t, database.UpsertSession(
		Session{ID: "sm-mixed", Project: "p", Machine: "m", Agent: "claude-code"}))
	_, err = database.getWriter().ExecContext(ctx,
		`UPDATE sessions SET created_at = 'garbage',
			ended_at = '2026-07-02T09:30:00.000Z' WHERE id = ?`, "sm-mixed")
	require.NoError(t, err)
	assert.Equal(t, "2026-07-02T09:30:00.000Z", readMarker("sm-mixed"))

	// The backfill twin applies the same rule.
	_, err = database.getWriter().ExecContext(ctx,
		`UPDATE sessions SET sync_marker = NULL WHERE id IN ('sm-malformed', 'sm-mixed')`)
	require.NoError(t, err)
	_, err = database.getWriter().ExecContext(ctx, backfillSyncMarkerSQL)
	require.NoError(t, err)
	assert.Empty(t, readMarker("sm-malformed"))
	assert.Equal(t, "2026-07-02T09:30:00.000Z", readMarker("sm-mixed"))
}
