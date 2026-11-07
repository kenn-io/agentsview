//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestDuckDBFrustrationMemoFollowsMirrorReplacement(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	local, path := newPushFixture(t, 2)
	for _, id := range []string{"sess-1", "sess-2"} {
		require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion}}))
	}
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, path)
	require.NoError(t, err)
	defer store.Close()

	resp, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Zero(t, resp.QualityHealth.Totals.FrustrationMarkerCount)
	var fingerprint string
	require.NoError(t, store.DB().QueryRowContext(ctx,
		`SELECT agentsview_push_fingerprint FROM sessions WHERE id = 'sess-1'`).Scan(&fingerprint))
	require.NotEmpty(t, fingerprint)
	store.frustrationMemo.Store(store.frustrationMemo.Generation(), "sess-1", fingerprint, 7)
	resp, err = store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 7, resp.QualityHealth.Totals.FrustrationMarkerCount)

	appendFrustrationPrompt(t, local, "sess-2")
	res, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsPushed)
	store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
	resp, err = store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 8, resp.QualityHealth.Totals.FrustrationMarkerCount)
}

func TestDuckDBFrustrationMemoSkipsUnwatchedStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	buildMirrorFixture(t, path, "session", "this is broken")
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	store := NewStoreFromDB(conn)
	defer store.Close()
	resp, err := store.GetAnalyticsSignals(t.Context(), db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	var fingerprint string
	require.NoError(t, store.DB().QueryRowContext(t.Context(),
		`SELECT agentsview_push_fingerprint FROM sessions`).Scan(&fingerprint))
	_, ok := store.frustrationMemo.Lookup(store.frustrationMemo.Generation(), "session", fingerprint)
	assert.False(t, ok)
}

func appendFrustrationPrompt(t *testing.T, local *db.DB, id string) {
	t.Helper()
	appendSessionMessage(t, local, id, "user", "this is broken",
		db.SessionSignalUpdate{QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion}})
}

func TestDuckDBFrustrationMemoSkipsMissingFingerprint(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	buildMirrorFixture(t, path, "session", "this is broken")
	conn, err := Open(ctx, path)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET agentsview_push_fingerprint = NULL`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err := NewStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	resp, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	_, cached := store.frustrationMemo.Lookup(store.frustrationMemo.Generation(), "session", "")
	assert.False(t, cached)
}
