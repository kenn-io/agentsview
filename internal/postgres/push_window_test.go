package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// The PG push selects candidates with the same unbounded-above
// ListSessionsForMirrorWindow the DuckDB mirror push uses (Backend Parity).
// The tests below pin the two behaviors the old bounded
// ListSessionsModifiedBetween(lastPush, now) selection got wrong:
//
//   - a clock-skewed future file_mtime pushed a session's sync_marker past
//     "now", so the upper-bounded query excluded it and later real changes
//     stayed stale in PG until wall time caught up;
//   - boundary-equal sessions (marker == lastPush) needed a separate
//     exclusive-lower re-query, which the inclusive mirror window subsumes.

func pushWindowSessionIDs(sessions []db.Session) []string {
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.ID)
	}
	return ids
}

func TestPushWindowSelectsFutureMarkerSession(t *testing.T) {
	local := testDB(t)
	ctx := context.Background()

	started := "2026-03-11T12:00:00Z"
	// Truncate to milliseconds so the nanosecond value round-trips exactly
	// through the trigger's ms-precision sync_marker text.
	futureMtime := time.Now().Add(48 * time.Hour).
		Truncate(time.Millisecond).UTC()
	futureNanos := futureMtime.UnixNano()
	require.NoError(t, local.UpsertSession(db.Session{
		ID:           "sess-future",
		Project:      "proj",
		Machine:      "m",
		Agent:        "claude",
		StartedAt:    &started,
		MessageCount: 1,
		FileMtime:    &futureNanos,
	}), "upsert future-mtime session")

	lastPush := time.Now().Add(-time.Hour).UTC().
		Format(LocalSyncTimestampLayout)
	cutoff := time.Now().UTC().Format(LocalSyncTimestampLayout)

	// The old bounded selection masked the session behind its future
	// file_mtime.
	bounded, err := local.ListSessionsModifiedBetween(
		ctx, lastPush, cutoff, nil, nil,
	)
	require.NoError(t, err, "bounded selection")
	assert.NotContains(t, pushWindowSessionIDs(bounded), "sess-future",
		"fixture must reproduce the masking the bounded query suffered")

	// The mirror window the push now uses selects it.
	window, err := local.ListSessionsForMirrorWindow(ctx, lastPush, nil, nil)
	require.NoError(t, err, "mirror window selection")
	assert.Contains(t, pushWindowSessionIDs(window), "sess-future",
		"the unbounded mirror window must select the future-marker session")

	// A subsequent real change must be selected on the next push even
	// after the watermark advanced to the current wall clock: the future
	// marker keeps the session a candidate in every later window, so the
	// changed fingerprint gets pushed instead of staying stale until wall
	// time catches up.
	require.NoError(t, local.UpsertSession(db.Session{
		ID:           "sess-future",
		Project:      "proj",
		Machine:      "m",
		Agent:        "claude",
		StartedAt:    &started,
		MessageCount: 2,
		FileMtime:    &futureNanos,
	}), "update future-mtime session")
	nextWindow, err := local.ListSessionsForMirrorWindow(ctx, cutoff, nil, nil)
	require.NoError(t, err, "mirror window after change")
	assert.Contains(t, pushWindowSessionIDs(nextWindow), "sess-future",
		"a later change to the future-marker session must be selected")
}

// TestPushWindowIncludesBoundaryEqualSession pins the inclusive lower bound
// that replaced the push's boundary-equal re-query: a session whose
// sync_marker exactly equals the last-push watermark must be re-selected by
// the primary window (the per-session fingerprint comparison then skips it
// when unchanged), while markers strictly below the watermark stay out.
func TestPushWindowIncludesBoundaryEqualSession(t *testing.T) {
	local := testDB(t)
	ctx := context.Background()

	boundary := time.Now().Add(time.Hour).Truncate(time.Millisecond).UTC()
	boundaryNanos := boundary.UnixNano()
	started := "2026-03-11T12:00:00Z"
	require.NoError(t, local.UpsertSession(db.Session{
		ID:           "sess-boundary",
		Project:      "proj",
		Machine:      "m",
		Agent:        "claude",
		StartedAt:    &started,
		MessageCount: 1,
		FileMtime:    &boundaryNanos,
	}), "upsert boundary session")

	atBoundary, err := local.ListSessionsForMirrorWindow(
		ctx, boundary.Format(LocalSyncTimestampLayout), nil, nil,
	)
	require.NoError(t, err, "window at boundary")
	assert.Contains(t, pushWindowSessionIDs(atBoundary), "sess-boundary",
		"marker == lastPush must be selected by the inclusive window")

	pastBoundary, err := local.ListSessionsForMirrorWindow(
		ctx,
		boundary.Add(time.Millisecond).Format(LocalSyncTimestampLayout),
		nil, nil,
	)
	require.NoError(t, err, "window past boundary")
	assert.NotContains(t, pushWindowSessionIDs(pastBoundary), "sess-boundary",
		"markers strictly below the watermark must stay out of the window")
}
