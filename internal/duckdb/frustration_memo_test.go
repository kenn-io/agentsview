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
	appendFrustrationPrompt(t, local, "sess-1")
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	conn, err := Open(ctx, path)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET frustration_markers_version = 0`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err := NewStore(ctx, path)
	require.NoError(t, err)
	defer store.Close()

	resp, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	var fingerprint string
	require.NoError(t, store.DB().QueryRowContext(ctx,
		`SELECT agentsview_push_fingerprint FROM sessions WHERE id = 'sess-1'`).Scan(&fingerprint))
	require.NotEmpty(t, fingerprint)
	count, cached := store.frustrationMemo.Lookup(store.frustrationMemo.Generation(), "sess-1", fingerprint)
	require.True(t, cached)
	assert.Equal(t, 1, count)

	appendFrustrationPrompt(t, local, "sess-2")
	res, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsPushed)
	store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
	count, cached = store.frustrationMemo.Lookup(store.frustrationMemo.Generation(), "sess-1", fingerprint)
	assert.True(t, cached, "replacement must retain unchanged transcript counts")
	assert.Equal(t, 1, count)
	resp, err = store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 2, resp.QualityHealth.Totals.FrustrationMarkerCount)
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
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET agentsview_push_fingerprint = NULL, frustration_markers_version = 0`)
	require.NoError(t, err)
	// A system row is not a prompt, so its marker text must not count.
	_, err = conn.ExecContext(ctx, `INSERT INTO messages (session_id, ordinal, role, content, is_system)
		SELECT session_id, 99, 'user', 'this is broken', true FROM messages LIMIT 1`)
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

func TestDuckDBStoredFrustrationCounts(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	local, path := newPushFixture(t, 2)
	appendFrustrationPrompt(t, local, "sess-1")
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)

	conn, err := Open(ctx, path)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE messages SET content = 'fine' WHERE session_id = 'sess-1' AND content = 'this is broken'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err := NewStore(ctx, path)
	require.NoError(t, err)
	resp, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	require.NoError(t, store.Close())

	appendFrustrationPrompt(t, local, "sess-2")
	result, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed)
	conn, err = Open(ctx, path)
	require.NoError(t, err)
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT frustration_marker_count FROM sessions WHERE id = 'sess-2'`).Scan(&count))
	assert.Equal(t, 1, count)
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET frustration_markers_version = 0 WHERE id = 'sess-1'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err = NewStore(ctx, path)
	require.NoError(t, err)
	defer store.Close()
	resp, err = store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
}

func TestDuckDBStoredFrustrationCountsMatchScanWithFilters(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	local := newLocalDB(t)
	const ts = "2026-02-01T09:00:00.000Z"
	messages := []db.Message{
		syncMessage("markers", 0, "user", "this is broken", ts),
		syncMessage("markers", 1, "user", "fine", ts),
		syncMessage("markers", 2, "assistant", "this is broken", ts),
		syncMessage("markers", 3, "user", "this is broken", ts),
		syncMessage("markers", 4, "user", "this is broken", ts),
	}
	messages[3].IsSystem = true
	messages[4].SourceSubtype = "tool_result"
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session:     syncSession("markers", "alpha", "this is broken", ts, len(messages)),
		Messages:    messages,
		Signals:     db.SessionSignalUpdate{QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion, ShortPromptCount: 3}},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	hour := 9
	filter := db.AnalyticsFilter{Model: "claude-test", Hour: &hour, Timezone: "UTC"}
	store, err := NewStore(ctx, path)
	require.NoError(t, err)
	stored, err := store.GetAnalyticsSignals(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 1, stored.QualityHealth.Totals.FrustrationMarkerCount)
	require.NoError(t, store.Close())
	conn, err := Open(ctx, path)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET frustration_markers_version = 0`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store, err = NewStore(ctx, path)
	require.NoError(t, err)
	defer store.Close()
	scanned, err := store.GetAnalyticsSignals(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, stored, scanned)
}
