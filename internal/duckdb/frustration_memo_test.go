//go:build !(windows && arm64)

package duckdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func appendFrustrationPrompt(t *testing.T, local *db.DB, id string) {
	t.Helper()
	appendSessionMessage(t, local, id, "user", "this is broken",
		db.SessionSignalUpdate{QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion}})
}

func TestDuckDBIncrementalPushUpdatesFrustrationCount(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)

	appendFrustrationPrompt(t, local, "sess-1")
	result, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, result.Diagnostics.Full)
	conn, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT frustration_marker_count FROM sessions WHERE id = 'sess-1'`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, conn.Close())
}

func TestDuckDBStoredFrustrationCounts(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	local, path := newPushFixture(t, 2)
	for _, id := range []string{"sess-1", "sess-2"} {
		require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion}}))
	}
	appendFrustrationPrompt(t, local, "sess-1")
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	conn, err := Open(ctx, path)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE messages SET content = CASE WHEN session_id = 'sess-2' THEN 'this is broken' ELSE 'fine' END WHERE role = 'user'`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET project = id`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err := NewStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	resp, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{Project: "sess-1"})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	zero, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{Project: "sess-2"})
	require.NoError(t, err)
	assert.Zero(t, zero.QualityHealth.Totals.FrustrationMarkerCount)
}
