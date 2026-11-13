//go:build !(windows && arm64)

package duckdb

import (
	"os"
	"path/filepath"
	"strconv"
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
	_, err = conn.ExecContext(ctx, `UPDATE sync_metadata SET value = '0' WHERE key = ?`, frustrationMarkersVersionMetadataKey)
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
	assert.Equal(t, 2, res.SessionsPushed)
	replacement := filepath.Join(filepath.Dir(path), "replacement.duckdb")
	require.NoError(t, os.Rename(path, replacement))
	setMirrorMetadataValue(t, replacement, frustrationMarkersVersionMetadataKey, "0")
	require.NoError(t, os.Rename(replacement, path))
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
	setMirrorMetadataValue(t, path, frustrationMarkersVersionMetadataKey, "0")
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
	_, err = conn.ExecContext(ctx, `UPDATE sync_metadata SET value = '0' WHERE key = ?`, frustrationMarkersVersionMetadataKey)
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

func TestDuckDBFrustrationMirrorVersion(t *testing.T) {
	skipReopenTestOnWindows(t)
	for _, tc := range []struct {
		name    string
		version int
		want    int
		future  bool
	}{
		{"current", frustrationMarkersVersion, 1, false},
		{"stale", frustrationMarkersVersion - 1, 0, false},
		{"future", frustrationMarkersVersion + 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			require.NoError(t, recordMetadataKey(ctx, conn, frustrationMarkersVersionMetadataKey, strconv.Itoa(tc.version)))
			require.NoError(t, conn.Close())
			store, err := NewStore(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			resp, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{Project: "sess-1"})
			if tc.future {
				require.ErrorContains(t, err, "newer than supported")
				_, err = store.GetAnalyticsSignalSessions(ctx, db.AnalyticsFilter{}, "frustration_marker_count", 10)
				require.ErrorContains(t, err, "newer than supported")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, resp.QualityHealth.Totals.FrustrationMarkerCount)
			if tc.name == "current" {
				zero, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{Project: "sess-2"})
				require.NoError(t, err)
				assert.Zero(t, zero.QualityHealth.Totals.FrustrationMarkerCount)
			}
		})
	}
}

func TestDuckDBFrustrationVersionFollowsSessionGeneration(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	buildMirrorFixture(t, path, "session", "this is broken")
	conn, err := Open(ctx, path)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET frustration_marker_count = 0`)
	require.NoError(t, err)
	require.NoError(t, recordMetadataKey(ctx, conn, frustrationMarkersVersionMetadataKey, "0"))
	require.NoError(t, conn.Close())
	store, err := NewStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sessions, err := store.loadAnalyticsSessions(ctx, db.AnalyticsFilter{}, false, false, "", nil)
	require.NoError(t, err)
	require.Len(t, sessions, 1)

	next := filepath.Join(dir, "next.duckdb")
	buildMirrorFixture(t, next, "session", "this is broken")
	require.NoError(t, swapMirrorFile(next, path))
	store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
	var version int
	require.NoError(t, store.queryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM sync_metadata WHERE key = ?`, frustrationMarkersVersionMetadataKey).Scan(&version))
	require.Equal(t, frustrationMarkersVersion, version)
	rows := []db.SignalRow{{ID: "session"}}
	require.NoError(t, store.duckPopulateFrustrationMarkers(ctx, rows, sessions))
	assert.Equal(t, 1, rows[0].FrustrationMarkerCount)
}

func TestDuckDBPushRejectsFutureFrustrationVersion(t *testing.T) {
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	future := strconv.Itoa(frustrationMarkersVersion + 1)
	setMirrorMetadataValue(t, path, frustrationMarkersVersionMetadataKey, future)
	before, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	for _, full := range []bool{false, true} {
		_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, full, nil)
		require.ErrorContains(t, err, "newer than supported")
		after, err := ProbeMirror(ctx, path)
		require.NoError(t, err)
		assert.Equal(t, before, after)
	}
	conn, err := Open(ctx, path)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `DROP TABLE image_text`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, true, nil)
	require.ErrorContains(t, err, "newer than supported")
	conn, err = OpenReadOnly(ctx, path)
	require.NoError(t, err)
	version, err := readMetadataKey(ctx, conn, frustrationMarkersVersionMetadataKey)
	require.NoError(t, err)
	assert.Equal(t, future, version)
	require.NoError(t, conn.Close())
}
