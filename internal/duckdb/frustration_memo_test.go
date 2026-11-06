//go:build !(windows && arm64)

package duckdb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func buildFrustrationMirror(t *testing.T, path, prompt string) {
	t.Helper()
	local := newLocalDB(t)
	ts := "2026-01-01T00:00:00.000Z"
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session:         syncSession("session", "alpha", prompt, ts, 1),
		Messages:        []db.Message{syncMessage("session", 0, "user", prompt, ts)},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	_, err = rebuildMirror(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, "test", nil)
	require.NoError(t, err)
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), "UPDATE sessions SET quality_signal_version = ?", db.CurrentQualitySignalVersion)
	require.NoError(t, err)
}

func TestDuckDBFrustrationMemoReuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	buildFrustrationMirror(t, path, "this is broken")
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()

	resp, err := store.GetAnalyticsSignals(t.Context(), db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	count, ok := store.frustrationMemo.Lookup(store.frustrationMemo.Generation(), "session", "")
	require.True(t, ok)
	assert.Equal(t, 1, count)
	store.frustrationMemo.Store(store.frustrationMemo.Generation(), "session", "", 7)
	resp, err = store.GetAnalyticsSignals(t.Context(), db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 7, resp.QualityHealth.Totals.FrustrationMarkerCount)
}

func TestDuckDBFrustrationMemoFollowsMirrorReplacement(t *testing.T) {
	skipReopenTestOnWindows(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")
	buildFrustrationMirror(t, path, "this is broken")
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	for range 2 {
		resp, err := store.GetAnalyticsSignals(t.Context(), db.AnalyticsFilter{})
		require.NoError(t, err)
		assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	}

	nextPath := filepath.Join(dir, "next.duckdb")
	buildFrustrationMirror(t, nextPath, "Please explain this function.")
	require.NoError(t, os.Rename(nextPath, path))
	store.checkMirrorReplacement(t.Context(), func(err error) { require.NoError(t, err) })
	resp, err := store.GetAnalyticsSignals(t.Context(), db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Zero(t, resp.QualityHealth.Totals.FrustrationMarkerCount)
}

func TestDuckDBFrustrationMemoSkipsUnwatchedStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	buildFrustrationMirror(t, path, "this is broken")
	conn, err := Open(t.Context(), path)
	require.NoError(t, err)
	store := NewStoreFromDB(conn)
	defer store.Close()
	resp, err := store.GetAnalyticsSignals(t.Context(), db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
	_, ok := store.frustrationMemo.Lookup(store.frustrationMemo.Generation(), "session", "")
	assert.False(t, ok)
}
